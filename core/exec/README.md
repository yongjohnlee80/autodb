# core/exec

`core/exec` is `autodb`'s central SQL execution engine. Every user interface—the PostgreSQL wire frontdoor, the interactive terminal UI (TUI), the JSON-RPC daemon, and script runners—executes queries, commands, and transactions through this package. No frontend communicates directly with target database drivers.

`core/exec` is responsible for statement classification, fine-grained authorization, admission pipeline gating, target database connection pooling, global socket permit enforcement, transaction and session lifecycles, and atomic audit logging.

---

## Architecture & Visual Diagrams

### 1. Statement Execution Pipeline

Every SQL statement submitted to `core/exec` passes through a strict multi-stage pipeline before touching a database connection:

```
                      [Incoming SQL String]
                                │
                                ▼
                   [1. Lexing & Classification]
                   (core/exec/classify.go)
                   • Real tokenizer: nested comments, strings, dollar-quotes
                   • Extract: Main Verb, FactClass (Read/Write/DDL/Control)
                   • Detect: WHERE clauses, function calls, CTEs
                   • Enforce: Exactly one statement per call
                                │
                                ▼
                     [2. Authorization Gate]
                      (core/auth/authz.go)
                   • Resolve caller identity and credentials fresh
                   • Calculate effective role: min(userRole, grantRole)
                   • Verify effective role >= required rank for action
                                │
                                ▼
                     [3. Admission Pipeline]
                    (core/admission/stage.go)
                   • Adapt facts and execution context into Orchestrator
                   • Run ordered stage checks (size, syntax, guards)
                   • First-deny-wins: halt on policy violation
                   • Suppress risk observations if statement is refused
                                │
                                ▼
               [4. Target Connection & Permit Acquisition]
                  (core/exec/conns.go, permits.go)
                   • Acquire per-target pooled or pinned connection
                   • Enforce aggregate targetPermits socket ledger
                   • Apply demand reclamation if pool is saturated
                                │
                                ▼
                     [5. Target Execution]
                   • Dispatch SQL to target driver (pgx/database/sql)
                   • Track execution timing and row count bounds
                   • Enforce cancellation and statement timeouts
                                │
                                ▼
                   [6. Result Paging & Streaming]
                   • Stream or buffer rows up to MaxRows page cap
                   • Capture query metadata and execution errors
                                │
                                ▼
                 [7. Audit & Outcome Recording]
                 (core/exec/txaudit.go, outcomes.go)
                   • Commit immutable security audit row (AuditTxCorrelated)
                   • Record history entry (if history is enabled)
                   • Project execution outcome to core/outcome registry
```

---

### 2. Connection Pooling, Target Permits & Sizing

To prevent backend database exhaustion across diverse workloads and multi-tenant frontdoor traffic, `core/exec` implements a two-tier connection bounding architecture:

```
  ┌─────────────────────────────────────────────────────────────┐
  │                   Host Target Database                      │
  │               (Maximum Sockets = MaxTargetBudget)           │
  └──────────────────────────────┬──────────────────────────────┘
                                 ▲
                     TCP Sockets │ (Enforced by targetPermits)
                                 │
  ┌──────────────────────────────┴──────────────────────────────┐
  │                 permits.go: permitLedger                    │
  │  Aggregate socket cap across ALL pools in this autodb instance│
  └──────────────┬───────────────────────────────┬──────────────┘
                 │                               │
                 ▼                               ▼
  ┌──────────────────────────────┐┌──────────────────────────────┐
  │   Target Pool: "prod-db"     ││   Target Pool: "analytics"   │
  │   poolMaxConns = 16          ││   poolMaxConns = 8           │
  │   Reserved Headroom = 4      ││   Reserved Headroom = 4      │
  │   (Interactive / TUI seats)  ││   (Interactive / TUI seats)  │
  └──────────────┬───────────────┘└──────────────┬───────────────┘
                 │                               │
        ┌────────┴────────┐             ┌────────┴────────┐
        ▼                 ▼             ▼                 ▼
   [Pooled Conns]   [Pinned Conns] [Pooled Conns]   [Pinned Conns]
    (Stateless       (Long-lived    (Stateless       (Long-lived
     Queries)         Wire / TX)     Queries)         Wire / TX)
```

#### Bounding Invariants
- **Per-Pool Cap (`poolMaxConns`)**: Bounds the number of connections opened to a specific target database.
- **Aggregate Ledger (`targetPermits`)**: An instance-wide atomic ticket counter that bounds the total number of simultaneous target sockets across all connection pools combined.
- **Reserved Headroom**: A calculated minimum number of connection slots (`min(4, pool / 2)`) reserved exclusively for interactive TUI operators and emergency admin queries, preventing frontdoor saturation from locking out operators.

---

### 3. Session & Pinned Transaction Lifecycle

Interactive TUI sessions and PostgreSQL wire sessions hold long-lived states and pinned multi-statement transactions:

```
                    [OpenSession / OpenWireSession]
                                   │
                                   ▼
                             [Active State]
                         (Holding Pinned Backend)
                                   │
                  Statement Run? ──┴── Client Inactive?
                        │                       │
                        ▼                       ▼
               [Executing Statement]   [Idle-in-Transaction]
                        │                       │
                        │               Duration > Timeout?
                        │                       │
                        │           YES ────────┴──────── NO
                        │            │                     │
                        │            ▼                     ▼
                        │      [Timeout Expired]       [Continue]
                        │      (Trigger Janitor)
                        │            │
                        ▼            ▼
                   [Quiescing / Demotion Phase]
                   • Wait for in-flight statement (closeQuiesce)
                   • Continuous Standing Authority Re-Check:
                     ResolveStanding(ctx, ref, user, conn)
                            │
              Standing? ────┴──── Revoked / Demoted?
                 │                         │
                 ▼                         ▼
         [Commit / Rollback]      StandingVerdict.MayWrite == false?
                 │                  │              │
                 │                 YES             NO (Revoked)
                 │                  │              │
                 │                  ▼              ▼
                 │             [Abort Write   [Terminate Session]
                 │              Transaction;
                 │              Keep Read-Only]
                 ▼                  │              │
        [Backend State Reset] ◄─────┴──────────────┘
        • DISCARD ALL / RESET ALL
        • Close Prepared Statements
        • Clear Session GUCs
        • Drop Temporary Tables
                 │
                 ▼
        [Return to Pool / Discard]
```

#### Session Safety Invariants
- **Comprehensive Backend Reset**: Before a pooled backend connection is released back into the pool, it executes a verified reset sequence (`DISCARD ALL`, clear temporary tables, reset session-level GUCs).
- **Graceful Quiescing**: Session termination waits up to `closeQuiesce` for active SQL execution to complete cleanly before forcibly severing backend sockets.
- **Continuous Standing Evaluation**: In-flight wire transactions periodically re-evaluate their authority via `ResolveStanding`. If a user is demoted from `editor` to `reader` during an open transaction, the transaction aborts write operations while keeping the read-only session alive.

---

### 4. PostgreSQL Wire Extended Query Protocol Pipeline

`core/exec` natively supports the PostgreSQL Extended Query Protocol (`Parse`, `Bind`, `Describe`, `Execute`, `Sync`) for JDBC, pgx, psycopg, and other compliant drivers:

```
  Wire Client                    core/exec/wire_extended.go            Target DB
       │                                     │                             │
       │── Parse ("stmt1", "SELECT ...") ───>│                             │
       │                                     │── Prepare Statement ───────>│
       │                                     │◄─ Statement Prepared ───────│
       │<─ ParseComplete ────────────────────│                             │
       │                                     │                             │
       │── Bind ("portal1", "stmt1", args) ─>│                             │
       │                                     │── Bind Parameters ─────────>│
       │                                     │◄─ Portal Created ───────────│
       │<─ BindComplete ─────────────────────│                             │
       │                                     │                             │
       │── Describe (Portal, "portal1") ────>│                             │
       │<─ RowDescription ───────────────────│ (Derived from cached plan)  │
       │                                     │                             │
       │── Execute ("portal1", maxRows) ────>│                             │
       │                                     │── Gated by Admission ──────>│
       │                                     │── Execute Portal ──────────>│
       │                                     │◄─ Data Rows Returned ───────│
       │<─ DataRow(s) / CommandComplete ─────│                             │
       │                                     │                             │
       │── Sync ────────────────────────────>│                             │
       │                                     │── Status Synchronization ──>│
       │<─ ReadyForQuery ────────────────────│                             │
```

---

## Domain Jargon

| Term | Definition |
| :--- | :--- |
| **Engine** | The central struct managing connection pools, query execution pipelines, session registries, and background reconcilers. |
| **Classifier** | A full lexical tokenizer that analyzes statement text, identifying verbs, clauses, dollar-quotes, and authorization classes. |
| **FactClass** | The coarsest authorization category of a statement: `ClassRead`, `ClassWrite`, `ClassDDL`, or `ClassControl`. |
| **Target Permits** | A global atomic ledger limiting total simultaneous outbound sockets across all managed database pools. |
| **Reserved Headroom** | Guaranteed connection slots set aside in each pool for interactive TUI operators to prevent lockout during traffic spikes. |
| **Pinned Backend** | A dedicated backend database socket leased exclusively to one wire session for the entire duration of a connection. |
| **Quiesce** | The controlled grace period during which a terminating session allows active statements to complete before forcing a disconnect. |
| **Extended Protocol** | PostgreSQL wire protocol flow separating query preparation (`Parse`), parameter binding (`Bind`), and execution (`Execute`). |
| **Demand Reclamation** | Automated reclamation of idle connections from background sessions when interactive callers experience queue starvation. |
| **Outcome Reconciler** | Background worker verifying that long-running transactions and interrupted executions are recorded accurately in audit logs. |

---

## Component & File Breakdown

| File Category | Files | Responsibilities |
| :--- | :--- | :--- |
| **Engine Core** | `engine.go`, `doc.go`, `profile.go`, `policy.go`, `settings.go` | Central `Engine` lifecycle, configuration options, capability profiles, and reloadable execution policies. |
| **Classification** | `classify.go`, `dialect.go`, `pgkeywords.go` | Full SQL tokenizer, verb identification, CTE analysis, WHERE clause detection, and statement count enforcement. |
| **Admission Integration** | `admission_facts.go`, `admission_stages.go`, `admission_drive.go` | Adapting statement facts to `core/admission`, running the stage pipeline, and applying admission rules. |
| **Connection Pooling** | `conns.go`, `dsn.go`, `permits.go`, `dial_failed.go`, `acquire_queue.go` | Target database connection pooling, DSN construction, permit ledger accounting, and driver error classification. |
| **Session Lifecycle** | `session.go`, `session_engine.go`, `session_tx.go`, `session_timeout.go`, `session_state.go` | Interactive session management, transaction pin/unpin, idle timeout sweeps, and quiesce teardown. |
| **Wire Protocol Engine** | `wire_session.go`, `wire_execute.go`, `wire_extended.go`, `wire_query.go`, `wire_txstatus.go` | PostgreSQL wire protocol support, extended query portals, parameter status reporting, and wire error rendering. |
| **Auditing & Outcomes** | `txaudit.go`, `txreconcile.go`, `history.go`, `outcomes.go`, `wait_outcome.go` | Immutable transaction audit logs, history tracking, and outcome registry declarations. |
| **Reclamation & Health** | `demand_reclaim.go`, `release_gate.go`, `cancel_registry.go`, `capacity_snapshot.go` | Demand-based connection reclamation, cancel key mapping, and capacity telemetry snapshots. |

---

## Security Invariants & Defenses

1. **Strict Single-Statement Enforcement**:
   - Multi-statement SQL strings (e.g. `SELECT 1; DROP TABLE users;`) are detected by the classifier and rejected immediately. Statement smuggling past the role gate is mathematically impossible.
2. **Tokenizer-Level Escaping & Quoting**:
   - The classifier uses a full lexical scanner supporting SQL standard string literals, C-style escapes, PostgreSQL dollar-quoted strings (`$$tag$$`), and nested block comments (`/* /* */ */`), preventing filter bypasses via token boundary tricks.
3. **Bounded Memory & Audit Footprints**:
   - Executable statements are capped at `maxStatementBytes` (default 64 KiB), while audit trails cap stored query text at `maxAuditSQLBytes` (8 KiB). An oversized query cannot exhaust engine RAM or flood metadata storage.
4. **Leak-Proof Connection Reset**:
   - A connection returning to a pool runs `DISCARD ALL` and verifies the absence of residual session parameters, preventing credential leakage or poisoned GUCs from crossing tenant boundaries.
5. **Fail-Closed Capability Enforcement**:
   - If an engine or target connection has an unrecognized capability profile, it fails closed by rejecting all statements rather than defaulting to a permissive posture.

---

## Go Usage Examples

### 1. Initializing the Engine

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/yongjohnlee80/autodb/core/auth"
    "github.com/yongjohnlee80/autodb/core/exec"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func newExecEngine(store *meta.Store, authSvc *auth.Service) (*exec.Engine, error) {
    engine := exec.New(store, authSvc,
        exec.WithMaxRows(500),
        exec.WithHistory(true),
        exec.WithPoolMaxConns(16),
        exec.WithProfile(exec.ProfileSession),
        exec.WithMaxStatementBytes(64*1024),
    )

    // Start background janitor to sweep expired sessions and idle transactions
    engine.StartJanitor(1 * time.Minute)

    return engine, nil
}
```

### 2. Executing a Simple Query

```go
func runQuery(ctx context.Context, engine *exec.Engine, token string, connID int64, sqlText string) error {
    // Engine automatically classifies, authorizes, admits, and audits the statement
    result, err := engine.Execute(ctx, token, connID, sqlText)
    if err != nil {
        return fmt.Errorf("execution failed: %w", err)
    }

    fmt.Printf("Query returned %d rows in %v\n", len(result.Rows), result.Duration)
    for _, row := range result.Rows {
        fmt.Println(row)
    }

    return nil
}
```
