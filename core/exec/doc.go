// Package exec is autodb's central SQL execution engine. It provides the
// unified execution pipeline through which every frontend—the PostgreSQL wire
// frontdoor, the interactive terminal UI (TUI), the JSON-RPC daemon, and script
// runners—submits queries, commands, and transactions against target databases.
// No frontend interacts directly with database drivers or raw connection pools.
//
// # Statement Execution Pipeline
//
// Every SQL statement executes through a structured, multi-phase pipeline:
//
//  1. Lexing & Classification: Statements are tokenized by a full lexical scanner
//     (classify.go) that handles nested block comments, dollar-quoted blocks,
//     escaped strings, and CTE prefixes. It extracts the statement verb, assigns
//     an authorization FactClass (read, write, ddl, control), detects WHERE
//     clauses and function calls, and enforces exactly one statement per call.
//
//  2. Authorization Gate: Caller credentials and tokens are re-resolved fresh
//     via core/auth on every execution. Effective privileges are evaluated as
//     min(userRole, grantRole), ensuring account roles strictly bound connection
//     grants.
//
//  3. Admission Gating: Statement facts and execution context are passed to
//     core/admission. The orchestrator executes an ordered chain of inspection
//     stages. Evaluation follows first-deny-wins semantics, immediately
//     halting execution on policy violations and suppressing risk telemetry.
//
//  4. Target Connection & Permit Acquisition: Statements acquire connections from
//     per-target pools subject to a global instance-wide socket ledger (targetPermits).
//     Dedicated headroom is reserved for interactive operators to prevent
//     frontdoor saturation from starving administrators.
//
//  5. Target Execution: Statements are dispatched to downstream database drivers
//     with active statement timeouts and cancellation registries.
//
//  6. Result Paging & Streaming: Query rows are buffered or streamed up to
//     configurable page caps (MaxRows), preventing unbounded memory growth.
//
//  7. Audit & Outcome Recording: Execution attempts, row counts, timings, and
//     outcomes are persisted into the immutable audit log (AuditTxCorrelated)
//     and optional history log. Outcomes are projected into the core/outcome
//     registry.
//
// # Connection Pooling & Socket Budgeting
//
// The engine manages backend connections using a two-tier bounding architecture:
//   - Per-Pool Limits (poolMaxConns): Restricts the maximum connections opened
//     to any single target database.
//   - Aggregate Socket Ledger (targetPermits): An instance-wide ticket counter
//     limiting total outbound sockets across all pools, preventing database host
//     exhaustion.
//   - Operator Headroom: Each pool guarantees reserved seats (min(4, pool / 2))
//     dedicated to interactive TUI sessions and emergency administrative queries.
//
// # Session & Pinned Transaction Management
//
// The engine distinguishes between stateless pooled queries and stateful pinned
// sessions:
//   - Pooled Executions: Run against shared connections in autocommit mode.
//     Connections are reset completely (DISCARD ALL) before returning to the pool.
//   - Pinned Sessions: Used by wire connections and multi-statement transactions.
//     The session leases a backend connection exclusively. Background janitor
//     sweeps enforce idle-in-transaction timeouts and maximum transaction duration
//     ceilings.
//   - Standing Authority: Pinned wire transactions continuously re-evaluate
//     authority via ResolveStanding. Role demotions gracefully abort in-flight
//     write transactions while keeping read-only client sessions connected.
//
// # PostgreSQL Wire Protocol Integration
//
// The engine provides first-class support for both the simple query protocol and
// the extended query protocol (Parse, Bind, Describe, Execute, Sync). Prepared
// statements and portals are cached per wire session, parameter statuses are
// observed and reported, and errors are rendered faithfully to PostgreSQL wire
// specifications.
package exec
