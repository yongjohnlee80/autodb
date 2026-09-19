# core/meta

`core/meta` is `autodb`'s management database subsystem and data access layer. It maintains the system's durable state of record: user identities, authentication keyslots, target connection credentials, workspace partitions, access grants, active sessions, personal access tokens (PATs), audit trails, execution history, and transaction outcome ledgers.

Built on `golib/dao`, the meta store supports two interchangeable storage backends with identical schema shapes:
1. **SQLite**: Embedded default for standalone workstations and local development, configured in WAL mode with busy timeouts and foreign key enforcement.
2. **PostgreSQL**: Production networked backend for high-availability deployments, protected by strict TLS validation (`sslmode=verify-full`) and connection pool bounds.

---

## Architecture & Visual Diagrams

### 1. Entity Relationships & Schema Graph

All metadata entities are managed via immutable, strongly typed `dao.Schema` instances:

```
  ┌─────────────────┐       ┌─────────────────┐
  │      User       │───────┤     Keyslot     │
  │ (Identity/Role) │       │ (Master KEK/DEK)│
  └────────┬────────┘       └─────────────────┘
           │
           ├─────────────────────────┬─────────────────────────┐
           │ 1:N                     │ 1:N                     │ 1:N
           ▼                         ▼                         ▼
  ┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
  │     Session     │       │      Grant      │       │     UserIP      │
  │ (Active Auth)   │       │(Role Assignment)│       │ (CIDR Allowlist)│
  └────────┬────────┘       └────────┬────────┘       └─────────────────┘
           │ 1:N                     │
           ▼                         │ References
  ┌─────────────────┐                ▼
  │       PAT       │       ┌─────────────────┐       ┌─────────────────┐
  │ (Bearer Token)  │       │    Workspace    │───────┤  WorkspaceConn  │
  └─────────────────┘       │ (Partition Boundary)    │  (Target Assoc) │
                            └─────────────────┘       └────────┬────────┘
                                                               │ References
                                                               ▼
  ┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
  │   HistoryEntry  │       │    AuditEntry   │       │   Connection    │
  │ (Executed SQL)  │       │ (Tamper Evidence│       │(Target DB DSNs) │
  └─────────────────┘       └─────────────────┘       └─────────────────┘

  ┌─────────────────┐       ┌─────────────────┐       ┌─────────────────┐
  │    TxOutcome    │       │    TxPending    │       │     MetaKV      │
  │(Settled Commits)│       │ (In-Flight Tx)  │       │(System Metadata)│
  └─────────────────┘       └─────────────────┘       └─────────────────┘
```

---

### 2. Dual-Engine Architecture & Open Pipeline

To prevent cyclic package dependencies, `core/meta` declares the `StoreConfig` interface, which is satisfied structurally by `config.Meta`. Neither package imports the other.

```
                           [Open(ctx, StoreConfig)]
                                      │
                                      ▼
                         [OpenNoMigrate(ctx, mcfg)]
                                      │
                       mcfg.StoreEngine() matches?
                                      │
                      SQLite ─────────┴───────── Postgres
                        │                            │
                        ▼                            ▼
               [openSqlite]                [postgres.OpenNamed]
               • Path: DefaultPath()       • DSN: mcfg.StoreDSN()
               • Mode: 0700 dir            • Pool: metaPoolBound(mcfg)
               • Pragmas: WAL, 5s timeout, • MaxConns: min 2 connections
                 foreign_keys enabled                │
                        │                            │
                        └─────────────┬──────────────┘
                                      │
                                      ▼
                        [runMigrations(ctx, conn)]
                        • Executes up-only SQL migrations
                        • Updates schema_migrations table
                                      │
                                      ▼
                         [Instantiate DAO Schemas]
                         • Users, Connections, Grants,
                           Workspaces, Sessions, Audit...
                                      │
                                      ▼
                              [Return *Store]
```

---

### 3. Instance Lease & Single-Daemon Enforcement

A meta store must be accessed by at most one running `autodb` daemon at any given time. Sharing a meta store across multiple daemons would lead to split-brain in-memory session registries and corrupted audit timelines.

`AcquireLease` claims exclusive process-lifetime ownership using operating system guarantees that vanish automatically if the process terminates, eliminating stale lockfile bugs:

```
                      [AcquireLease(ctx, store, mcfg)]
                                     │
                      Store Engine is SQLite or Postgres?
                                     │
                     SQLite ─────────┴───────── Postgres
                       │                            │
                       ▼                            ▼
              [acquireFileLease]            [acquirePGLease]
              • Opens <path>.lock           • Dedicated transaction connection
              • syscall.Flock(LOCK_EX)      • pg_try_advisory_xact_lock(key)
                       │                            │
            Lock held? ─┴─ Lock held?    Lock held? ─┴─ Lock held?
                │              │             │              │
               YES             NO           YES             NO
                │              │             │              │
                ▼              ▼             ▼              ▼
           [ErrLeaseHeld]   [Admit]     [ErrLeaseHeld]   [Spawn Heartbeat]
                                                         • Periodic ping
                                                         • Signals Lost() channel
                                                           if connection drops
```

---

### 4. SQLite-to-PostgreSQL Online Migration (`MigrateToPostgres`)

`MigrateToPostgres` provides a verified, one-way copy of an entire SQLite meta-store into a PostgreSQL database:

```
  [1. Validate Target Operational Rules]
  • CheckOperational: Verify sslmode=verify-full and pool floor >= 2
                 │
                 ▼
  [2. Open Connections Without Schema Mutation]
  • OpenNoMigrate on SQLite and PostgreSQL
                 │
                 ▼
  [3. Acquire Target Instance Lease]
  • Guarantees no other daemon is currently serving the PostgreSQL store
                 │
                 ▼
  [4. Execute Destination Migrations]
  • Brings PostgreSQL schema up to current migration version
                 │
                 ▼
  [5. Topological Entity Stream Transfer]
  • Stream copy within target transaction in foreign key order:
    KV -> Users -> Keyslots -> Connections -> Workspaces ->
    WorkspaceConns -> Grants -> AllowedIPs -> UserIPs ->
    Sessions -> PATs -> History -> Audit -> TxOutcomes -> TxPending
                 │
                 ▼
  [6. Row Count & Integrity Audit]
  • Confirms 100% record parity between source and destination
```

---

## Jargon & Core Concepts

| Term | Definition |
| :--- | :--- |
| **Instance Lease** | A process-scoped mutual exclusion lock (`flock` on SQLite, transaction advisory lock on PostgreSQL) preventing multiple daemons from operating on the same meta store. |
| **StoreConfig** | Consumer-defined structural interface (`StoreEngine`, `StorePath`, `StoreDSN`, `StorePoolMaxConns`) used to open the store without importing `core/config`. |
| **Partition** | A security boundary bounding entity access to a specific workspace and its permitted connection associations. |
| **TxOutcome** | Durable journal record documenting the settling state (`committed`, `rolled_back`, `unknown`) of client transactions for audit and crash recovery. |
| **Tombstone** | A pruned transaction outcome retaining terminal state while freeing intermediate statement transition history. |
| **Keyslot** | Master cryptographic key envelope holding wrapped encryption keys for user authentication and connection secret storage. |

---

## Entity Schema Directory

| Entity | Go Struct | Primary Key | Key Indexes & Constraints | Purpose |
| :--- | :--- | :--- | :--- | :--- |
| `users` | `User` | `int64` (id) | `name` (UNIQUE), `role` | Operator & reader identities |
| `connections` | `Connection` | `int64` (id) | `name` (UNIQUE), `engine` | Target database connection profiles |
| `workspaces` | `Workspace` | `int64` (id) | `name` (UNIQUE) | Administrative security partitions |
| `workspace_connections` | `WorkspaceConn` | `int64` (id) | `(workspace_id, connection_id)` UNIQUE | Associations between workspaces and targets |
| `grants` | `Grant` | `int64` (id) | `(user_id, workspace_id)` UNIQUE | Role-based partition permissions |
| `sessions` | `Session` | `int64` (id) | `token` (UNIQUE), `user_id` | Interactive TUI and RPC bearer sessions |
| `pats` | `PAT` | `int64` (id) | `token_hash` (UNIQUE), `user_id` | Personal Access Tokens for frontdoor clients |
| `history` | `HistoryEntry` | `int64` (id) | `user_id`, `created_at` | Executed query recall per user |
| `audit` | `AuditEntry` | `int64` (id) | `user_id`, `timestamp`, `action` | Tamper-evident operational audit logs |
| `tx_outcomes` | `TxOutcome` | `int64` (id) | `tx_id` (UNIQUE), `status` | Resolved transaction outcome tracking |
| `tx_pending` | `TxPending` | `int64` (id) | `tx_id` (UNIQUE) | In-flight transactions requiring reconciliation |
| `allowed_ips` | `AllowedIP` | `int64` (id) | `cidr` (UNIQUE) | Server-wide RPC network allowlists |
| `user_ips` | `UserIP` | `int64` (id) | `(user_id, cidr)` UNIQUE | Per-user network admission allowlists |
| `store_meta` | `MetaKV` | `string` (key)| `key` (UNIQUE) | Internal versioning & engine flags |
| `keyslots` | `Keyslot` | `string` (slot)| `slot` (UNIQUE) | Encrypted master keyslot storage |

---

## Portability & Scan Contracts

To ensure complete query and data scan interoperability between SQLite and PostgreSQL, `core/meta` enforces strict cross-engine storage conventions:
- **Identifiers**: Signed 64-bit integers (`int64`), mapped to `INTEGER PRIMARY KEY AUTOINCREMENT` in SQLite and `BIGSERIAL` / `BIGINT` in PostgreSQL.
- **Timestamps**: Expressed strictly as Unix seconds in integer columns (`INTEGER` / `BIGINT`). Database-native timestamp types are avoided to eliminate timezone parse discrepancies.
- **Booleans**: Stored as numeric `0` or `1` integer flags (`INTEGER` / `SMALLINT`).
- **Enumerations**: Stored as `TEXT` with strict SQL `CHECK` constraints to ensure database-level validation.

---

## Usage Examples

### Opening Store & Acquiring Lease

```go
package main

import (
    "context"
    "log"

    "github.com/yongjohnlee80/autodb/core/config"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func main() {
    ctx := context.Background()
    cfg, err := config.Load("")
    if err != nil {
        log.Fatalf("Config load failed: %v", err)
    }

    // Open store and bring schema to latest version
    store, err := meta.Open(ctx, cfg.Meta)
    if err != nil {
        log.Fatalf("Failed to open meta store: %v", err)
    }
    defer store.Close()

    // Claim exclusive single-daemon instance lease
    lease, err := meta.AcquireLease(ctx, store, cfg.Meta)
    if err != nil {
        log.Fatalf("Failed to acquire store lease: %v", err)
    }
    defer lease.Release()

    log.Printf("Meta store opened successfully on %s", store.Engine())
}
```

### Querying Entities with Type-Safe DAO Schemas

```go
func findUserByLogin(ctx context.Context, store *meta.Store, username string) (*meta.User, error) {
    user, err := store.Users.OnCtx(ctx).
        With(meta.UserName, username).
        Get()
    if err != nil {
        return nil, err
    }
    return user, nil
}
```

### Reading & Writing Metadata KV Entries

```go
func recordSchemaFlag(ctx context.Context, store *meta.Store, key, val string) error {
    return store.SetMeta(ctx, key, val)
}
```
