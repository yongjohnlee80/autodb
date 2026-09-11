# Package `admission` — SQL Statement Admission Pipeline

`admission` is autodb's **statement admission pipeline** (ADR-0096). It defines the protocol-neutral vocabulary and orchestration engine for inspecting, analyzing, and gating SQL statements before they are permitted to execute against a database backend.

---

## 1. Architectural Overview

The admission pipeline sits directly between SQL classification and query execution:

```
[Inbound Client Request]
           │
           ▼
[Statement Classifier & Lexer] (core/exec)
           │  Extracts Facts (Verb, Class, Nesting Depth, AST Shapes, GUCs)
           ▼
[Admission Orchestrator] (core/admission)
           │  Evaluates ordered Stages against Facts & Context
           ├── Stage 1: Script Size Bound
           ├── Stage 2: Predicate Guard (Top-Level WHERE)
           ├── Stage 3: Routine Catalog Check (User Code Execution)
           └── Stage 4: Session State & GUC Allowlist
           │
           ├──► [Denial Issued] ──► Query Refused (Report.PrimaryDeny)
           │
           └──► [All Stages Admitted]
                       │
                       ▼
             [Database Backend Execution]
```

---

## 2. Core Jargon & Glossary

- **`Facts`**: The static, structural properties of a statement extracted by the parser/lexer (e.g. main verb, authorization class, whether a WHERE clause exists at paren depth 0, nested subquery mutations).
- **`Context`**: The dynamic runtime environment of the execution (e.g. transport physical context, caller read-only status, target database capabilities, open transaction state).
- **`PhysicalCtx`**: The transport surface:
  - `PhysPooled`: Stateless pooled connection.
  - `PhysSession`: Stateful interactive IPC/TUI session.
  - `PhysWire`: Dedicated PostgreSQL TCP wire connection.
- **`TargetCaps`**: Capabilities exposed by the target database (e.g., `CapRoutineCatalog` for Postgres-family catalog introspection, `CapTxReadOnly` for server-enforced read-only transactions).
- **`Needs`**: The prerequisite conditions declared by a stage. If the context and facts do not satisfy a stage's needs, the stage is skipped ("absence by construction").
- **`Stage`**: A discrete inspection check implementing `admission.Stage`.
- **`Contribution`**: The outcome returned by a stage: either a deny `Reason`, a risk `Observation`, or empty.
- **`Reason`**: A structured denial describing the violated policy `Code`, error `Class`, character `Span`, human-readable `Detail`, actionable `Hint`, and whether the session can `Continue`.
- **`Observation`**: A non-blocking telemetry finding recorded for admitted queries (e.g. high-complexity join or unbounded limit).
- **`Report`**: The aggregate evaluation result produced by the `Orchestrator`.

---

## 3. Key Design Patterns & Invariants

### 3.1 Leaf Isolation & Zero Wire Knowledge
`admission` imports **nothing** from `core/exec`, `frontdoor`, or database drivers. It knows nothing of PostgreSQL v3 wire packets or SQLSTATE strings. Frontends map protocol-neutral `Reason.Code` values to their own transport errors (e.g. SQLSTATE `42501` for permission denied).

### 3.2 Two-Return Design: Policy vs Operational Failures
`Stage.Apply` returns `(Contribution, error)`:
- `Contribution`: Policy decision (denial or risk).
- `error`: Operational breakdown (e.g., catalog query failed, memory error).

Operational errors abort the pipeline immediately as an `*OperationalError`. They are never confused with statement rejections.

### 3.3 Mandatory Disclosure of Deny Codes
Every stage must declare all possible denial `Code`s it can emit via `DenyCodes()`. The `Orchestrator` strictly validates this at evaluation time: if a stage denies with an undeclared code, execution aborts with an operational error. This guarantees that audit loggers and client error renderers can discover all possible refusal codes via `Orchestrator.Registered()`.

### 3.4 Stop-on-First-Deny
The pipeline evaluates stages in strict sequential order and halts at the first denial. The first denial is the root cause; presenting multiple secondary errors confuses user remediation.

---

## 4. Stage Implementation Guide

To implement a new admission stage, implement the `Stage` interface:

```go
type MyCustomStage struct{}

func (s *MyCustomStage) Name() string {
    return "my-custom-stage"
}

func (s *MyCustomStage) ContextNeeds() admission.Needs {
    return admission.Needs{
        // Declare preconditions here
        ReadOnlyUnit: true,
    }
}

func (s *MyCustomStage) DenyCodes() []admission.Code {
    return []admission.Code{
        "custom-policy-violation",
    }
}

func (s *MyCustomStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
    if facts.Verb() == "DROP" {
        return admission.Deny(admission.Reason{
            Code:     "custom-policy-violation",
            Class:    admission.ClassPermission,
            Subject:  "DROP",
            Detail:   "DROP statements are not permitted for read-only units",
            Continue: true,
        }), nil
    }
    return admission.NoContribution(), nil
}
```

---

## 5. Usage Example

```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/admission"
)

func main() {
    // 1. Compose stages into an ordered chain
    orchestrator := admission.Compose(
        &SizeBoundStage{MaxBytes: 65536},
        &PredicateGuardStage{},
        &SessionStateStage{},
    )

    // 2. Prepare facts (from SQL parser) and execution context
    var facts admission.Facts = mockFacts{verb: "DELETE", hasWhere: false}
    ctx := admission.Context{
        Phys:     admission.PhysPooled,
        ReadOnly: false,
    }

    // 3. Evaluate through the orchestrator
    report, err := orchestrator.Run(facts, ctx)
    if err != nil {
        log.Fatalf("admission pipeline broke: %v", err)
    }

    // 4. Inspect report
    if report.IsDenied() {
        reason, _ := report.PrimaryDeny()
        fmt.Printf("Query rejected: [%s] %s\n", reason.Code, reason.Detail)
        return
    }

    fmt.Println("Query admitted for execution.")
}
```
