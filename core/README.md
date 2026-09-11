# Package `core` — The Package of Record

`core` is **autodb's single package of record**. It houses all business logic, security policies, cryptographic operations, access control, query admission gates, execution pipelines, and audit persistence.

Every frontend surface—the Terminal UI (`tui`), the Neovim/Lua plugin (`lua/autodb`), the Browser Web UI (`webserver`), the local RPC daemon (`rpc`), and the PostgreSQL wire-protocol proxy (`frontdoor`)—is a thin transport adapter. **No business or security logic is permitted to exist outside `core`.**

---

## Architecture Diagram

```
+-------------------------------------------------------------------------+
|                           FRONTEND SURFACES                             |
|                                                                         |
|   +-------------------+  +------------------+  +--------------------+   |
|   | Terminal UI (TUI) |  | Neovim / autovim |  | Browser Web UI     |   |
|   | (Bubble Tea / TUI)|  | (RPC Socket)     |  | (WebSocket Proxy)  |   |
|   +---------+---------+  +--------+---------+  +---------+----------+   |
|             |                     |                      |              |
|             |            +--------+---------+            |              |
|             |            | RPC Server (IPC) |            |              |
|             |            +--------+---------+            |              |
|             |                     |                      |              |
|             +------------------+  |  +-------------------+              |
|                                |  |  |                                  |
|   +----------------------------v--v--v------------------------------+   |
|   | Front Door Wire Proxy (PostgreSQL v3 wire-protocol daemon)     |   |
|   +---------------------------------+-------------------------------+   |
+-------------------------------------|-----------------------------------+
                                      |
============= CONSUMES (Thin Transports, Zero Business Logic) =============
                                      |
+-------------------------------------v-----------------------------------+
|                               PACKAGE core                              |
|                                                                         |
|  +--------------------+   +------------------------------------------+  |
|  |    core/config     |   |                core/auth                 |  |
|  |  * TOML Config     |   |  * Keyslot Storage (AES-256-GCM)         |  |
|  |  * Endpoints       |   |  * Argon2id Passwords & PAT Tokens       |  |
|  |  * DSN Resolution  |   |  * Sessions & IP Allowlists              |  |
|  +---------+----------+   |  * RBAC Standing (reader/editor/admin)   |  |
|            |              +--------------------+---------------------+  |
|            |                                   |                        |
|  +---------v----------+                        |                        |
|  |     core/meta      |                        |                        |
|  |  * Relational DB   |                        |                        |
|  |  * Migrations      |                        |                        |
|  |  * Audit Records   |                        |                        |
|  +---------+----------+                        |                        |
|            |                                   |                        |
|            +------------------+  +-------------+                        |
|                               |  |                                      |
|                     +---------v--v---------+                            |
|                     |      core/exec       |                            |
|                     |  * Connection Pools  |                            |
|                     |  * Transaction State |                            |
|                     |  * Query Execution   |                            |
|                     |  * Wire Sessions     |                            |
|                     +---------+------------+                            |
|                               |                                         |
|            +------------------+------------------+                      |
|            |                                     |                      |
|  +---------v----------+               +----------v---------+            |
|  |   core/admission   |               |    core/engine     |            |
|  |  * Lexer / AST     |               |  * Dialects (PG/MY)|            |
|  |  * Predicate Guards|               |  * Capabilities    |            |
|  |  * GUC Allowlist   |               |  * Engine Names    |            |
|  |  * Stage Pipeline  |               +--------------------+            |
|  +--------------------+                                                 |
+-------------------------------------------------------------------------+
                                      |
========================== CONNECTS TO TARGETS ============================
                                      |
                 +--------------------+--------------------+
                 |                                         |
       +---------v----------+                    +---------v----------+
       | Target Database    |                    | Meta Storage       |
       | (PostgreSQL, MySQL)|                    | (PostgreSQL/SQLite)|
       +--------------------+                    +--------------------+
```

---

## Security Principles & Invariants

1. **Zero-Bypass Gate Stack**: No statement can touch a target database without passing through authentication, connection grant verification, admission pipeline screening, and audit logging.
2. **Secret Zero (Envelope Encryption)**: Database connection strings (DSNs) are encrypted at rest using AES-256-GCM keyslots. Cleartext passwords never touch disk or configuration files.
3. **Three-Tier RBAC with Per-Connection Granularity**:
   - `reader < editor < admin`.
   - Global admin privileges do **not** imply unrestricted access to all connections. An admin still requires an explicit connection grant to execute queries on production targets.
4. **Server-Enforced Read-Only Transactions**: Read-only queries and reader accounts run inside real database read-only transactions (`SET TRANSACTION READ ONLY`). Even if a query smuggles a write past application checks (e.g., via stored functions), the database engine itself rejects it with SQLSTATE `25006`.
5. **Deterministic Statement Admission (ADR-0096)**: The admission pipeline inspects SQL structure upfront, enforcing predicate guards (rejecting `UPDATE` or `DELETE` without a top-level `WHERE`), inspecting GUCs, and blocking unsafe constructs before execution.

---

## Subpackages Overview

| Package | Responsibility |
|---|---|
| [`core/config`](config/) | TOML configuration file parsing, schema validation, endpoint defaults, and sizing bounds. |
| [`core/meta`](meta/) | Metadata store (PostgreSQL/SQLite) tracking users, connections, grants, migrations, and durable audit logs. |
| [`core/auth`](auth/) | Cryptography (AES-256-GCM, Argon2id), keyslots, sessions, PAT tokens, CIDR allowlists, and RBAC evaluation. |
| [`core/engine`](engine/) | Engine type definitions (`Postgres`, `MySQL`, `SQLite`) and target capability bitsets (`routine-catalog`, `tx-readonly`). |
| [`core/admission`](admission/) | Protocol-neutral statement admission framework: `Facts`, `Context`, `Stage`, and `Orchestrator` (ADR-0096). |
| [`core/exec`](exec/) | Connection pool management, transaction lifecycles, query execution, streaming results, and PostgreSQL wire protocol emulation. |

---

## End-to-End Request Flow

```
Client SQL Request
       │
       ▼
[1. Authentication & Session Validation] (core/auth)
       │  • Verify session token or Personal Access Token (PAT).
       │  • Enforce IP/CIDR allowlists.
       ▼
[2. Authorization & Grant Resolution] (core/auth, core/meta)
       │  • Re-resolve caller's role (reader/editor/admin) and per-connection grant.
       │  • Never cache authorization across calls.
       ▼
[3. Statement Classification & Admission] (core/admission)
       │  • Extract statement Facts (verb, class, AST shape, top-level WHERE, GUCs).
       │  • Execute ordered Stage pipeline via Orchestrator.
       │  • First denial halts execution and yields a structured Reason.
       ▼
[4. Connection Acquisition & Lease] (core/exec, core/meta)
       │  • Check out pooled connection or pin backend for wire session.
       │  • Open server-enforced read-only transaction if caller is a reader.
       ▼
[5. Target Execution] (core/exec)
       │  • Execute SQL on target engine with bounded timeout and cancellation support.
       │  • Stream row batches or command tags to client.
       ▼
[6. Durable Audit Persistence] (core/meta)
       │  • Log actor, client IP, connection, sanitized SQL prefix, latency, and disposition.
       ▼
Client Result Response
```

---

## Glossary of Core Terms

- **DSN (Data Source Name)**: The connection URI containing target database host, port, user, password, and database name.
- **Keyslot**: An encrypted container storing a connection's DSN using AES-256-GCM. Unlocked at startup with a master passphrase or key file.
- **Standing**: The resolved authorization status of a user on a given connection, combining global role with per-connection grants.
- **Admission Stage**: A discrete inspection unit implementing `admission.Stage` (e.g., verifying `WHERE` clauses on mutations, validating GUC settings).
- **Contribution**: The outcome returned by an admission stage: either a deny `Reason`, a risk `Observation`, or empty.
- **GUC (Grand Unified Configuration)**: PostgreSQL session-level parameters modified via `SET` (e.g. `statement_timeout`, `search_path`).
- **Physical Context (`PhysicalCtx`)**: The transport environment of an execution: `PhysPooled` (stateless pool), `PhysSession` (IPC/TUI session), or `PhysWire` (dedicated wire connection).

---

## Usage Example

```go
package main

import (
    "context"
    "log"

    "github.com/yongjohnlee80/autodb/core/auth"
    "github.com/yongjohnlee80/autodb/core/config"
    "github.com/yongjohnlee80/autodb/core/exec"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func main() {
    ctx := context.Background()

    // 1. Load TOML configuration
    cfg, err := config.LoadFile("autodb.toml")
    if err != nil {
        log.Fatalf("failed to load config: %v", err)
    }

    // 2. Initialize the meta-store repository
    metaStore, err := meta.Open(ctx, cfg.MetaDSN())
    if err != nil {
        log.Fatalf("failed to open meta store: %v", err)
    }
    defer metaStore.Close()

    // 3. Initialize authentication and keyslot service
    authSvc := auth.NewService(metaStore, cfg.Auth)

    // 4. Initialize the execution engine
    execEngine, err := exec.NewEngine(metaStore, authSvc, cfg.Exec)
    if err != nil {
        log.Fatalf("failed to initialize execution engine: %v", err)
    }
    defer execEngine.Close()

    // Frontends (RPC, TUI, Frontdoor Wire Proxy) now drive execEngine.
}
```

---

## Licensing

Licensed under the [Apache-2.0 License](../LICENSE).

