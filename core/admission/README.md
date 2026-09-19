# core/admission

`core/admission` is `autodb`'s statement admission and inspection engine. It defines the leaf vocabulary and evaluation pipeline that decides whether a SQL statement is permitted to execute, what risks it presents, and what restrictions apply.

The admission engine sits between statement classification and statement execution. It operates without any dependency on network transports, wire protocols, or database execution internals.

---

## Architecture & Visual Diagrams

### 1. Admission Pipeline & Evaluation Flow

The `Orchestrator` coordinates an ordered chain of `Stage` implementations. Statements are evaluated sequentially; evaluation stops at the first denying stage ("first-deny-wins"), ensuring that the primary blocker is surfaced rather than obscured by secondary symptoms.

```
          [Incoming Statement]
                   │
                   ▼
     [Facts Extraction & Context Build]
    (AST/Lexer Facts + Connection Context)
                   │
                   ▼
       [Orchestrator.Run(facts, ctx)]
                   │
  ┌────────────────┴────────────────┐
  │ For Each Stage in Pipeline:     │
  │                                 │
  │  1. Check Applicability:        │
  │     applicable(stage, facts, ctx)
  │            │                    │
  │     NO ────┴──── YES            │
  │      │            │             │
  │  (Skip Stage)     ▼             │
  │            [applyStage(stage)]  │
  │            • Panic Trap Defer   │
  │            • stage.Apply()      │
  │                   │             │
  │             Panicked? ──YES──> [OperationalError(&PanicError)]
  │                   │            (Abort Pipeline Immediately)
  │                   ▼ NO          │
  │            Did Apply Fail?      │
  │                   │             │
  │            YES ───┴─── NO       │
  │             │          │        │
  │             ▼          ▼        │
  │    [OperationalError] [Inspect Contribution]
  │    (Abort Pipeline)    │        │
  │                        ▼        │
  │                 Deny != nil?    │
  │                        │        │
  │              YES ──────┴────── NO
  │               │                │
  │               ▼                ▼
  │       Is Code Declared in   Risk != nil?
  │       stage.DenyCodes()?       │
  │               │           YES ─┴─ NO
  │          NO ──┴── YES      │      │
  │           │        │       ▼      ▼
  │           ▼        │  [Append  (Next Stage)
  │     [Operational   │   Risk]
  │        Error]      ▼
  │               [Report.Deny]
  │               (HALT PIPELINE)
  └────────────────────┬────────────┘
                       │
                       ▼
                 [Final Report]
         IsDenied() / PrimaryDeny() / Risk
```

---

### 2. Leaf Decoupling & Boundary Architecture

`core/admission` is a strict leaf package. It imports no protocol handlers, no wire formats, no database drivers, and no execution engine types. Dependency arrows point exclusively inward toward `core/admission`.

```
  ┌───────────────────────────────────────────────────────────┐
  │                       Callers / Surfaces                  │
  │                                                           │
  │   [frontdoor]           [rpc]                [tui]        │
  │  (Postgres Wire)    (Daemon API)       (Terminal UI)      │
  └─────────┬─────────────────┬────────────────────┬──────────┘
            │                 │                    │
            ▼                 ▼                    ▼
  ┌───────────────────────────────────────────────────────────┐
  │                       core/engine                         │
  │  • Adapts SQL statements to admission.Facts               │
  │  • Assembles connection state into admission.Context      │
  │  • Composes stages into admission.Orchestrator            │
  └───────────────────────────┬───────────────────────────────┘
                              │
                              ▼
  ┌───────────────────────────────────────────────────────────┐
  │                     core/admission                        │
  │  (Leaf Vocabulary: Facts, Context, Stage, Orchestrator)   │
  │                                                           │
  │  • No knowledge of SQLSTATE or wire protocols             │
  │  • No knowledge of execution plans or disk engines        │
  │  • Pure policy evaluation: Contribution (Deny vs Risk)   │
  └───────────────────────────────────────────────────────────┘
```

#### Boundary Invariants
- **No Reverse Dependencies**: `core/admission` never imports `core/exec`, `core/engine`, `frontdoor`, or `rpc`.
- **Pure Data In / Pure Data Out**: Inputs are `Facts` (an accessor interface) and `Context` (a value struct). Output is `Report` (a value struct).
- **Transport Independence**: Error codes and denial reasons are rendered into protocol-specific structures (e.g. PostgreSQL wire error packets, JSON-RPC errors) exclusively at the perimeter surfaces, never inside admission.

---

### 3. Stage Applicability & Filtering Architecture

A stage declares what context and facts it requires via `ContextNeeds()`. If the runtime context cannot supply the declared needs, the orchestrator skips the stage entirely. This guarantees **absence by construction**: an inapplicable stage never runs and cannot inadvertently return partial or misleading contributions.

```
                        [applicable(stage, facts, ctx)]
                                       │
                                       ▼
                     Does stage need ReadOnlyUnit?
                                       │
                         YES ──────────┴────────── NO
                          │                         │
                          ▼                         ▼
                    ctx.ReadOnly?          Does stage need ControlVerb?
                     (false -> SKIP)                │
                          │            YES ─────────┴───────── NO
                          ▼             │                       │
                          └────────────►│                       ▼
                                        ▼                 Does stage need SetShape?
                                facts.Class() == Control?       │
                                     (false -> SKIP)       YES ─┴─ NO
                                        │                   │      │
                                        ▼                   ▼      ▼
                                        └──────────────────►│ Does SetTarget Exist?
                                                            │   (false -> SKIP)
                                                            ▼      │
                                                            └─────►▼
                                                     Does stage need OnSession?
                                                                   │
                                                      YES ─────────┴───────── NO
                                                       │                       │
                                                       ▼                       ▼
                                                ctx.Phys == Session     Does target have
                                                 or ctx.Phys == Wire?   stage.TargetCaps?
                                                  (false -> SKIP)              │
                                                       │                  YES ─┴─ NO
                                                       ▼                   │      │
                                                       └──────────────────►│      ▼
                                                                           │   [SKIP]
                                                                           ▼
                                                                      [APPLICABLE]
```

#### Declarative Requirements (`Needs`)
- **`OnSession`**: Affirmative check (`ctx.Phys == PhysSession || ctx.Phys == PhysWire`). Never "not pooled". Unset or zero-value transports fail closed toward absence.
- **`ReadOnlyUnit`**: Stage applies only to reader execution units.
- **`ControlVerb`**: Stage applies only when `facts.Class() == ClassControl`.
- **`SetShape`**: Stage requires parsed `SET` target details in `facts.SetTarget()`.
- **`TargetCaps`**: Stage requires specific target database capabilities (e.g. `CapRoutineCatalog`, `CapTxReadOnly`).

---

### 4. Error & Outcome Taxonomy: Policy vs Operational

`core/admission` enforces a strict semantic boundary between policy decisions and operational failures:

```
                            [Evaluation Outcome]
                                     │
           ┌─────────────────────────┴─────────────────────────┐
           ▼                                                   ▼
   [Policy Decisions]                                 [Operational Failures]
  (Returned as Contribution)                          (Returned as Go error)
           │                                                   │
     ┌─────┴─────────────────┐                           ┌─────┴─────────────────┐
     ▼                       ▼                           ▼                       ▼
   Deny                    Risk                  OperationalError            PanicError
 (Reason struct)      (Observation struct)     (Stage execution failed)   (Stage panicked)
     │                       │                           │                       │
     ▼                       ▼                           ▼                       ▼
Rejection of bad       Telemetry finding        Internal system blip       Captured stack trace
statement (e.g.        (e.g. complex join,      (e.g. catalog read         wrapped safely in
missing WHERE)         unindexed lookup)        failure on engine)         OperationalError
```

#### Crucial Distinctions
- **A Denial is NOT an Error**: A refusal indicates that the statement violated admission policy (e.g. `CodeNoWhere`). It is returned in `Contribution.Deny`.
- **An Error is NOT a Denial**: An error indicates that the stage could not run (e.g. database catalog read timeout, nil pointer panic). It is returned as `OperationalError`. Conflating an operational error with a denial would penalize clients for internal system faults.
- **Mandatory Disclosure**: Every denying stage must declare all possible denial codes in `DenyCodes()`. If a stage returns an undeclared code at runtime, `Orchestrator.Run` immediately rejects it with an `OperationalError`.

---

## Domain Jargon

| Term | Definition |
| :--- | :--- |
| **Facts** | The static, structural properties of a parsed statement (main verb, class, mutations, function calls, SET targets). |
| **Context** | The dynamic environment of the evaluation (physical transport, connection profile, read-only policy, target capabilities). |
| **PhysicalCtx** | Transport classification: `PhysPooled` (stateless pool), `PhysSession` (Unix socket / TUI), `PhysWire` (PostgreSQL wire connection). |
| **TargetCaps** | Bitset of capabilities supported by the downstream database (e.g. `CapRoutineCatalog`, `CapTxReadOnly`). |
| **Stage** | A modular inspection step in the admission pipeline that evaluates `Facts` in a `Context` and produces a `Contribution`. |
| **Contribution** | The output of a single stage: either a policy denial (`Deny`), a risk observation (`Risk`), or empty (`NoContribution`). |
| **Orchestrator** | Composes an ordered sequence of stages and executes them with panic containment and first-deny-wins semantics. |
| **First-Deny-Wins** | Pipeline execution halts at the first encountered denial, ensuring actionable and non-conflicting feedback to callers. |
| **Risk Suppression** | When a statement is denied, all risk observations are suppressed because the statement will not execute. |
| **Absence by Construction** | Stages whose declared `Needs` are not met by the runtime context are skipped entirely without runtime probing. |

---

## Component & File Breakdown

| File | Responsibilities |
| :--- | :--- |
| `doc.go` | Package-level documentation, architectural philosophy, boundary invariants, and design principles. |
| `facts.go` | The `Facts` accessor interface, authorization classes (`ClassRead`, `ClassWrite`, `ClassDDL`, `ClassControl`), `Mutation`, and `Call` structs. |
| `context.go` | The `Context` value struct, `PhysicalCtx` enum, `TargetCaps` bitset, and `Needs` specification struct. |
| `stage.go` | The `Stage` and `Analyzer` interfaces, refusal `Reason`, `Code` constants, `Contribution`, and `Report` structs. |
| `orchestrator.go` | Pipeline composition (`Compose`), sequential execution (`Run`), applicability filtering (`applicable`), and panic containment (`applyStage`). |
| `outcomes.go` | Projection of registered stage denial codes into system outcome registrations (`core/outcome`). |

---

## Go Usage Examples

### 1. Implementing a Custom Admission Stage

```go
package main

import (
    "fmt"

    "github.com/yongjohnlee80/autodb/core/admission"
)

// NoDropTableStage prevents DROP TABLE statements on production connections.
type NoDropTableStage struct{}

func (NoDropTableStage) Name() string {
    return "no-drop-table"
}

func (NoDropTableStage) ContextNeeds() admission.Needs {
    // Only applies to DDL statements
    return admission.Needs{}
}

func (NoDropTableStage) DenyCodes() []admission.Code {
    return []admission.Code{admission.CodeStatementUnsupported}
}

func (s NoDropTableStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
    if facts.Verb() == "DROP" {
        return admission.Deny(admission.Reason{
            Code:     admission.CodeStatementUnsupported,
            Class:    admission.ClassPermission,
            Subject:  "DROP TABLE",
            Detail:   "DROP statements are not permitted on production connections",
            Continue: true, // Session remains alive
        }), nil
    }
    return admission.NoContribution(), nil
}
```

### 2. Composing and Running an Orchestrator Chain

```go
func evaluateStatement(facts admission.Facts, ctx admission.Context) error {
    // Build an ordered pipeline
    pipeline := admission.Compose(
        NoDropTableStage{},
        // additional stages...
    )

    // Execute statement through the chain
    report, err := pipeline.Run(facts, ctx)
    if err != nil {
        if admission.IsOperationalError(err) {
            // Internal admission failure (e.g. stage panic or catalog error)
            return fmt.Errorf("admission system failure: %w", err)
        }
        return err
    }

    if report.IsDenied() {
        primary, _ := report.PrimaryDeny()
        return fmt.Errorf("statement refused: %s (code: %s)", primary.Detail, primary.Code)
    }

    // Process risk observations for telemetry
    for _, obs := range report.Risk {
        log.Printf("Admitted statement risk observation: %s: %s", obs.Code, obs.Detail)
    }

    return nil
}
```
