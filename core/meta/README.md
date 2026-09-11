# Package `meta` — Relational Metadata Repository

`meta` is autodb's **metadata and state persistence engine**. It manages the storage of user accounts, database connection profiles, access grants, workspaces, active sessions, query history, transaction recovery records, and the immutable security audit log.

---

## 1. Architectural Overview & Entity Graph

```
                   +------------------------------------+
                   |            StoreConfig             |
                   |   (Engine: SQLite or Postgres)     |
                   +-----------------+------------------+
                                     │
                                     ▼
                   +------------------------------------+
                   |             meta.Store             |
                   +-----------------+------------------+
                                     │
     ┌───────────────────────────────┼───────────────────────────────┐
     ▼                               ▼                               ▼
[Identity & Security]     [Connections & Grants]           [Audit & Recovery]
• Users                   • Connections                    • Audit
• Sessions                • Workspaces                     • History
• PATs                    • WorkspaceConns                 • TxOutcomes
• AllowedIPs / UserIPs    • Grants                         • TxPending
• Keyslots (AES-256)      • KV Settings
```

---

## 2. Dual-Engine Storage Strategy

`meta` supports two database engines with identical data contracts:

1. **SQLite (Embedded Mode)**:
   - Zero-config default for local developer workflows.
   - File path defaults to `$XDG_DATA_HOME/autodb/meta.db`.
   - Requires zero external services or daemon setup.

2. **PostgreSQL (Networked Production Mode)**:
   - Production clustering, high availability, and concurrent daemon scaling.
   - Configured via `[meta] engine = "postgres"` with strict `sslmode=verify-full`.
   - Supports native declarative table partitioning for audit trails.

---

## 3. Cross-Dialect Portability Invariants

To eliminate subtle driver divergence between SQLite (`modernc.org/sqlite`) and PostgreSQL (`jackc/pgx`):

- **Primary Keys**: Integer autoincrement `int64` IDs everywhere.
- **Timestamps**: Stored strictly as 64-bit Unix seconds in integer columns (`BIGINT` in Postgres, `INTEGER` in SQLite), avoiding timezone formatting divergence.
- **Booleans**: Stored as `0` or `1` integers.
- **Enums & Types**: Stored as `TEXT` with strict SQL `CHECK` constraints (e.g. `CHECK (role IN ('reader', 'editor', 'admin'))`).
- **Immutable Schemas**: Each table is wrapped in an immutable typed `golib/dao.Schema` struct.

---

## 4. Migrations & Safe Upgrades

- **`meta.Open(ctx, cfg)`**: Connects to the database and automatically applies all pending forward migrations up to the current binary version.
- **`meta.OpenNoMigrate(ctx, cfg)`**: Connects *without* mutating the schema. Used exclusively by administrative CLIs to take a distributed migration lease before altering tables.
- **Distributed Lease Locking**: Prevents concurrent migration races during rolling deployments using database-level advisory locks.
- **Live SQLite -> PostgreSQL Migration**: `MigrateToPostgres` copies an existing SQLite database into a target PostgreSQL database while verifying row counts and table checksums.

---

## 5. Glossary of Terms

- **`Store`**: The opened metadata repository struct holding schema handles for all entities.
- **`Keyslot`**: Stored row holding the AES-256-GCM encrypted master key wrapped per user or service.
- **`TxOutcome`**: Durable outcome record (committed, aborted, ambiguous) used by transaction recovery workers.
- **`Lease`**: A distributed lock record preventing multiple instances from executing migrations or batch tasks concurrently.
- **`Partitioning`**: Rolling date-range partitioning (monthly) for `audit` and `history` tables in PostgreSQL.

---

## 6. Usage Example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/config"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func main() {
    ctx := context.Background()

    cfg, err := config.Load()
    if err != nil {
        log.Fatalf("config error: %v", err)
    }

    // Open store and run pending migrations
    store, err := meta.Open(ctx, cfg.MetaConfig())
    if err != nil {
        log.Fatalf("failed to open meta store: %v", err)
    }
    defer store.Close()

    // Query active connections
    conns, err := store.Connections.List(ctx)
    if err != nil {
        log.Fatalf("failed to list connections: %v", err)
    }

    fmt.Printf("Meta store active with %d connections\n", len(conns))
}
```
