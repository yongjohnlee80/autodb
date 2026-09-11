// Package core is autodb's single package-of-record.
//
// All business logic, security policy, credential encryption, identity management,
// authorization, statement admission filtering, connection pooling, SQL execution,
// and audit logging live strictly within this package and its subpackages. Every
// frontend consumer (the Terminal UI, the Neovim/Lua plugin, the RPC daemon, the
// browser Web UI, and the PostgreSQL wire-protocol front door) is a thin transport
// layer that delegates 100% of policy and execution to core. No security or
// database logic exists outside core.
//
// ============================================================================
// SYSTEM ARCHITECTURE & TOPOLOGY
// ============================================================================
//
//	+-------------------------------------------------------------------------+
//	|                           FRONTEND SURFACES                             |
//	|                                                                         |
//	|   +-------------------+  +------------------+  +--------------------+   |
//	|   | Terminal UI (TUI) |  | Neovim / autovim |  | Browser Web UI     |   |
//	|   | (Bubble Tea / TUI)|  | (RPC Socket)     |  | (WebSocket Proxy)  |   |
//	|   +---------+---------+  +--------+---------+  +---------+----------+   |
//	|             |                     |                      |              |
//	|             |            +--------+---------+            |              |
//	|             |            | RPC Server (IPC) |            |              |
//	|             |            +--------+---------+            |              |
//	|             |                     |                      |              |
//	|             +------------------+  |  +-------------------+              |
//	|                                |  |  |                                  |
//	|   +----------------------------v--v--v------------------------------+   |
//	|   | Front Door Wire Proxy (PostgreSQL v3 wire-protocol daemon)     |   |
//	|   +---------------------------------+-------------------------------+   |
//	+-------------------------------------|-----------------------------------+
//	                                      |
//	============= CONSUMES (Thin Transports, Zero Business Logic) =============
//	                                      |
//	+-------------------------------------v-----------------------------------+
//	|                               PACKAGE core                              |
//	|                                                                         |
//	|  +--------------------+   +------------------------------------------+  |
//	|  |    core/config     |   |                core/auth                 |  |
//	|  |  * TOML Config     |   |  * Keyslot Storage (AES-256-GCM)         |  |
//	|  |  * Endpoints       |   |  * Argon2id Passwords & PAT Tokens       |  |
//	|  |  * DSN Resolution  |   |  * Sessions & IP Allowlists              |  |
//	|  +---------+----------+   |  * RBAC Standing (reader/editor/admin)   |  |
//	|            |              +--------------------+---------------------+  |
//	|            |                                   |                        |
//	|  +---------v----------+                        |                        |
//	|  |     core/meta      |                        |                        |
//	|  |  * Relational DB   |                        |                        |
//	|  |  * Migrations      |                        |                        |
//	|  |  * Audit Records   |                        |                        |
//	|  +---------+----------+                        |                        |
//	|            |                                   |                        |
//	|            +------------------+  +-------------+                        |
//	|                               |  |                                      |
//	|                     +---------v--v---------+                            |
//	|                     |      core/exec       |                            |
//	|                     |  * Connection Pools  |                            |
//	|                     |  * Transaction State |                            |
//	|                     |  * Query Execution   |                            |
//	|                     |  * Wire Sessions     |                            |
//	|                     +---------+------------+                            |
//	|                               |                                         |
//	|            +------------------+------------------+                      |
//	|            |                                     |                      |
//	|  +---------v----------+               +----------v---------+            |
//	|  |   core/admission   |               |    core/engine     |            |
//	|  |  * Lexer / AST     |               |  * Dialects (PG/MY)|            |
//	|  |  * Predicate Guards|               |  * Capabilities    |            |
//	|  |  * GUC Allowlist   |               |  * Engine Names    |            |
//	|  |  * Stage Pipeline  |               +--------------------+            |
//	|  +--------------------+                                                 |
//	+-------------------------------------------------------------------------+
//	                                      |
//	========================== CONNECTS TO TARGETS ============================
//	                                      |
//	                 +--------------------+--------------------+
//	                 |                                         |
//	       +---------v----------+                    +---------v----------+
//	       | Target Database    |                    | Meta Storage       |
//	       | (PostgreSQL, MySQL)|                    | (PostgreSQL/SQLite)|
//	       +--------------------+                    +--------------------+
//
// ============================================================================
// CORE PHILOSOPHY & SECURITY INVARIANTS
// ============================================================================
//
// 1. Zero-Bypass Gate Stack
//    No code path exists to execute a database query without traversing the full
//    authentication, connection-level grant check, statement admission pipeline,
//    and durable audit recorder. All frontends speak to the same core interfaces.
//
// 2. Secret Zero: No Cleartext Credentials at Rest
//    Target database passwords and DSN strings are never stored in cleartext.
//    Credentials are encrypted at rest using AES-256-GCM envelope encryption
//    within the keyslot subsystem (core/auth). Decryption keys reside only in
//    transient memory of the running process and are wiped on shutdown.
//
// 3. Three-Tier RBAC with Per-Connection Granularity
//    Authorization evaluates three roles: reader < editor < admin.
//    Crucially, connection-scoped actions require an explicit grant even for
//    admins. An admin without an editor/admin grant on connection "prod-db"
//    cannot run mutating queries on that connection.
//
// 4. Server-Enforced Read-Only Transactions
//    For read-only operations and reader accounts, autodb does not merely check
//    the SQL string via regex. It provisions server-enforced read-only
//    transactions (e.g., SET TRANSACTION READ ONLY in PostgreSQL). Even if a
//    malicious write is smuggled through dynamic SQL or stored procedures, the
//    underlying database engine aborts the transaction with SQLSTATE 25006.
//
// 5. Deterministic Statement Admission
//    Before any query reaches a backend connection, it is inspected by the
//    admission pipeline (core/admission). Dangerous statements (e.g. UPDATE or
//    DELETE without a top-level WHERE clause, unapproved session configuration
//    SET commands, unauthorized procedural executions) are rejected upfront
//    with structured, protocol-neutral reasons.
//
// ============================================================================
// REQUEST EXECUTION LIFECYCLE
// ============================================================================
//
// When a SQL request arrives from any frontend:
//
//	  Client Request (SQL text, Connection ID, User Session)
//	                         │
//	                         ▼
//	  [1. Authentication & Session Validation] (core/auth)
//	      Verify session token or PAT, check IP allowlists, resolve user identity.
//	                         │
//	                         ▼
//	  [2. Authorization & Grant Check] (core/auth, core/meta)
//	      Re-resolve permissions fresh per Execute (never cached across requests).
//	      Verify caller's role meets the connection's assigned grant level.
//	                         │
//	                         ▼
//	  [3. Statement Classification & Admission Pipeline] (core/admission)
//	      Parse statement facts (verb, class, AST shape, top-level WHERE, GUCs).
//	      Evaluate ordered admission stages (script size, predicate checks, etc.).
//	      If any stage denies, stop immediately and return structured Reason.
//	                         │
//	                         ▼
//	  [4. Connection Acquisition & Lease] (core/exec, core/meta)
//	      Obtain pooled connection or pinned wire-session backend.
//	      Enforce transaction isolation and read-only mode if required.
//	                         │
//	                         ▼
//	  [5. Target Execution] (core/exec)
//	      Execute statement on target database with timeout and cancel contexts.
//	      Stream tabular rows or command tags back to client.
//	                         │
//	                         ▼
//	  [6. Audit Trail Recording] (core/meta)
//	      Durably persist audit record: actor, client IP, connection, sanitized
//	      SQL text prefix, execution duration, row count, and error disposition.
//
// ============================================================================
// SUBPACKAGE RESPONSIBILITIES
// ============================================================================
//
//   - core/config:
//     Parsing and schema validation of TOML configuration files. Computes
//     endpoint connection strings, default filesystem paths, and sizing limits.
//
//   - core/meta:
//     The relational meta-store repository. Encapsulates migrations, database
//     connections, user records, access grants, audit logs, and keyslot blobs.
//
//   - core/auth:
//     Cryptographic and authorization services: AES-256-GCM keyslot encryption,
//     Argon2id password hashing, Personal Access Tokens (PATs), CIDR allowlists,
//     session tokens, and role-based access control (RBAC).
//
//   - core/engine:
//     Type declarations for database engines (Postgres, MySQL, SQLite) and
//     per-engine capability bitsets (routine catalog, read-only transaction support).
//
//   - core/admission:
//     Protocol-neutral SQL admission pipeline. Defines statement
//     Facts, evaluation Context, Stage interfaces, Contribution outcomes (Deny/Risk),
//     and the Orchestrator that halts on the first violated policy.
//
//   - core/exec:
//     SQL execution engine. Manages connection pooling, transaction lifecycles,
//     session timeouts, statement cancellation, streaming query results, and
//     PostgreSQL v3 wire-protocol session emulation for front-door clients.
package core

