// Package exec is autodb's core SQL execution and connection management engine.
//
// Every frontend (Terminal UI, Neovim/Lua plugin, Web UI, RPC server, and the
// Front Door wire proxy) routes through this package to execute statements against
// target databases. No frontend interacts directly with database drivers.
//
// ============================================================================
// SQL EXECUTION PIPELINE ARCHITECTURE
// ============================================================================
//
//	Client Query (SQL, Connection ID, Session/PAT Token)
//	                         │
//	                         ▼
//	   [1. Lexer & Statement Classifier] (classify.go)
//	       • Tokenizes SQL: comments, strings, dollar-quoting, CTEs.
//	       • Extracts statement Facts: verb, class, nesting depths, WHERE at D0.
//	                         │
//	                         ▼
//	   [2. Standing Authority Re-Check] (core/auth)
//	       • Verifies session/PAT validity fresh per statement.
//	       • Resolves effective permission floor (role + connection grant).
//	                         │
//	                         ▼
//	   [3. Statement Admission Pipeline] (admission_stages.go, core/admission)
//	       • Evaluates ordered admission stages.
//	       • Size bounds -> Predicate guards -> Routine checks -> GUC gates.
//	       • Halts immediately on first violation with structured Reason.
//	                         │
//	                         ▼
//	   [4. Target Connection Checkout & Lease] (conns.go, dsn.go)
//	       • Check out pooled backend or pin backend for wire session.
//	       • Enforce server-side read-only transaction (SET TRANSACTION READ ONLY)
//	         and statement timeouts (SET statement_timeout).
//	                         │
//	                         ▼
//	   [5. Target Execution & Row Streaming] (engine.go, wire_query.go)
//	       • Dispatches SQL to target database driver (Postgres/MySQL).
//	       • Streams tabular rows or command tags with cancellation propagation.
//	                         │
//	                         ▼
//	   [6. Durable Audit & History Logging] (txaudit.go, core/meta)
//	       • Persists execution attempt: actor, client IP, connection ID,
//	         sanitized SQL prefix, duration, row count, and error disposition.
//
// ============================================================================
// KEY EXECUTION SUBSYSTEMS
// ============================================================================
//
//   - Engine (engine.go):
//     Coordinates connection pools, auth re-checks, admission evaluation,
//     statement execution, and audit logging.
//
//   - Lexer & Classifier (classify.go):
//     A hand-written, deterministic tokenizer that classifies SQL statements
//     without full parser overhead, safely handling nested comments, CTEs,
//     dollar-quoted strings, and complex subqueries.
//
//   - Connection Manager (conns.go):
//     Maintains per-target connection pools, lazy initialization, connection
//     leases, and dynamic drain/eviction on credential rotation.
//
//   - Transaction State (session_tx.go, txcontrol.go, txreconcile.go):
//     Manages transaction lifecycles, idle-in-transaction timeouts, and
//     commit status reconciliation after network failure.
//
//   - Wire Protocol Emulation (wire_session.go, wire_query.go, wire_extended.go):
//     Full PostgreSQL v3 wire-protocol handler allowing native PostgreSQL clients
//     (psql, DBeaver, pgcli) to connect directly through the Front Door proxy.
package exec

