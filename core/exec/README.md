# Package `exec` — Query Execution, Connection Management & Wire Emulation

`exec` is autodb's **SQL execution and connection lifecycle engine**. Every frontend surface (Terminal UI, Neovim/Lua plugin, Web UI, RPC server, and Front Door proxy) routes through `exec` to query target databases. **No frontend code interacts directly with database drivers.**

---

## 1. Execution Pipeline Architecture

```
Client Query (SQL, Connection ID, Session/PAT Token)
                         │
                         ▼
   [1. Lexer & Statement Classifier] (classify.go)
       • Tokenizes SQL: comments, strings, dollar-quoting, CTEs.
       • Extracts statement Facts: verb, class, nesting depths, WHERE at D0.
                         │
                         ▼
   [2. Standing Authority Re-Check] (core/auth)
       • Verifies session/PAT validity fresh per statement.
       • Resolves effective permission floor (role + connection grant).
                         │
                         ▼
   [3. Statement Admission Pipeline] (admission_stages.go, core/admission)
       • Evaluates ordered admission stages (ADR-0096).
       • Size bounds -> Predicate guards -> Routine checks -> GUC gates.
       • Halts immediately on first violation with structured Reason.
                         │
                         ▼
   [4. Target Connection Checkout & Lease] (conns.go, dsn.go)
       • Check out pooled backend or pin backend for wire session.
       • Enforce server-side read-only transaction (SET TRANSACTION READ ONLY)
         and statement timeouts (SET statement_timeout).
                         │
                         ▼
   [5. Target Execution & Row Streaming] (engine.go, wire_query.go)
       • Dispatches SQL to target database driver (Postgres/MySQL).
       • Streams tabular rows or command tags with cancellation propagation.
                         │
                         ▼
   [6. Durable Audit & History Logging] (txaudit.go, core/meta)
       • Persists execution attempt: actor, client IP, connection ID,
         sanitized SQL prefix, duration, row count, and error disposition.
```

---

## 2. Key Subsystems

### 2.1 Lexer & Classifier (`classify.go`)
A deterministic, hand-written SQL tokenizer that operates without full AST parser overhead:
- Strips comments (including nested block comments `/* ... /* ... */ ... */`).
- Handles engine-specific string escape rules (e.g. MySQL backslash escapes vs PostgreSQL standard conforming strings).
- Supports PostgreSQL dollar-quoted strings (`$$` and `$tag$`).
- Classifies statement verbs into `FactClass` tiers (`read`, `write`, `ddl`, `control`).
- Tracks parenthesis nesting depth to ensure mutations (e.g. `UPDATE`, `DELETE`) have an authentic top-level `WHERE` clause at depth 0 rather than in an inner subquery.

### 2.2 Admission Pipeline Integration (`admission_stages.go`)
Adapts the protocol-neutral `core/admission` framework:
- **Intake Bound**: Blocks oversized payloads before classification (`script-too-large`).
- **Predicate Guard**: Enforces mandatory predicates on DML mutations (`mutation-without-predicate`).
- **Routine Catalog**: Prevents unauthorized procedural code execution in read-only sessions (`reader-advanced-pattern`).
- **Session State & GUCs**: Protects connection state by rejecting unauthorized `SET` and `LOCK` operations.

### 2.3 Connection Pools & Leasing (`conns.go`)
- Maintains per-target connection pools keyed by connection ID.
- Decrypts target DSNs on-demand from `core/auth` keyslots.
- Supports transparent pool eviction and draining when credentials rotate.
- Enforces pool concurrency bounds based on target capacity.

### 2.4 Transaction Management & Recovery (`session_tx.go`, `txreconcile.go`)
- Tracks active transaction states and enforces `idle_in_transaction_timeout`.
- Implements two-phase commit status reconciliation: if a network failure occurs during `COMMIT`, the reconciler queries target status oracles to determine whether the transaction committed or rolled back.

### 2.5 PostgreSQL Wire Protocol Emulation (`wire_session.go`, `wire_query.go`, `wire_extended.go`)
- Implements the PostgreSQL v3 wire protocol (Simple Query and Extended Query protocols).
- Allows standard tools (`psql`, DBeaver, DataGrip) to connect to autodb's Front Door proxy while enforcing the full autodb security stack.

---

## 3. Glossary of Terms

- **`FactClass`**: Statement category (`ClassRead`, `ClassWrite`, `ClassDDL`, `ClassControl`).
- **`Lease`**: An active checkout of a database connection from the pool.
- **`Pinned Transaction`**: A backend connection held exclusively by a stateful session until `COMMIT` or `ROLLBACK`.
- **`Audit SQL Prefix`**: Bounded text prefix (up to 8 KiB) stored in audit history to balance forensic visibility with storage overhead.
- **`Reconciliation`**: Recovery process resolving ambiguous in-flight transactions after client disconnects.

---

## 4. Usage Example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/auth"
    "github.com/yongjohnlee80/autodb/core/exec"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func main() {
    ctx := context.Background()

    var (
        store   *meta.Store
        authSvc *auth.Service
    )

    // Initialize execution engine
    engine, err := exec.NewEngine(store, authSvc, exec.Options{
        MaxStatementBytes: 65536,
        DefaultMaxRows:    500,
    })
    if err != nil {
        log.Fatalf("failed to create engine: %v", err)
    }
    defer engine.Close()

    // Execute query via engine
    res, err := engine.Execute(ctx, exec.Request{
        ConnID:      1,
        SessionID:   100,
        SQL:         "SELECT id, name FROM users WHERE active = true",
        ClientIP:    "127.0.0.1",
    })
    if err != nil {
        log.Fatalf("execution error: %v", err)
    }

    fmt.Printf("Query returned %d rows in %v\n", len(res.Rows), res.Duration)
}
```
