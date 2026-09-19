// Package admission defines the leaf vocabulary and evaluation pipeline for
// statement admission in autodb. It governs whether a SQL statement is
// permitted to execute, what structural risks it presents, and what operational
// restrictions apply.
//
// The admission engine sits between statement classification and database
// execution. It does not know the database wire protocol, transport framing,
// or SQLSTATE error encodings; outer surfaces (the PostgreSQL frontdoor, the
// RPC daemon, the interactive TUI) translate admission decisions into their
// respective protocols.
//
// # Architecture & Leaf Decoupling
//
// Admission is designed as an isolated leaf package:
//   - Dependency Direction: Admission never imports core/exec, core/engine,
//     frontdoor, or rpc. All dependencies point inward toward admission.
//   - Pure Accessor Interfaces: The Facts interface provides read-only
//     accessors (Verb, Class, Mutations, Calls, SetTarget) rather than binding
//     to internal engine AST or parser structs.
//   - Pure Value Contexts: The Context struct captures execution conditions
//     (PhysicalCtx, TargetCaps, Profile, ReadOnly, MayWrite, TxOpen) as immutable
//     snapshots built per evaluation.
//
// # The Stage Pipeline & First-Deny Semantics
//
// The Orchestrator composes an ordered sequence of Stage implementations.
// Evaluation proceeds sequentially through the chain:
//
//  1. Applicability Filtering: Before a stage runs, the orchestrator checks its
//     declared Needs against the statement's Facts and runtime Context. Stages
//     whose prerequisites are not satisfied are skipped (absence by construction).
//
//  2. Panic Containment: Each stage executes under an isolated defer/recover
//     boundary in applyStage. A stage panic is captured as a PanicError and
//     wrapped in an OperationalError, preventing engine crashes and identifying
//     the exact failing stage.
//
//  3. First-Deny Termination: When a stage contributes a denial (Reason), the
//     orchestrator halts evaluation immediately. The first denial is the primary
//     blocker; evaluating subsequent stages would produce misleading secondary
//     symptoms.
//
//  4. Risk Suppression: A denied statement drops any accumulated risk
//     observations. Because the statement will not execute, its risk profile
//     is immaterial; the refusal itself is the authoritative disposition.
//
// # Separation of Policy vs Operational Errors
//
// Admission enforces a strict separation between policy decisions and operational
// failures:
//   - Contribution represents policy: a denial (Deny *Reason) or a telemetry
//     finding (Risk *Observation). It is never returned as a Go error.
//   - Error represents operational failure: an internal failure within a stage
//     (e.g. catalog read timeout, nil pointer exception). It is returned as an
//     OperationalError and aborts the pipeline.
//
// Confusing an operational error with a denial would penalize clients for
// server faults; confusing a denial with an operational error would conceal
// policy violations.
//
// # Mandatory Disclosure
//
// Every Stage that can refuse a statement must declare all its refusal codes
// via DenyCodes(). The orchestrator verifies that any returned denial code was
// pre-declared; returning an undeclared denial code is treated as a disclosure
// violation and fails loudly with an OperationalError.
//
// # Outcome Projections
//
// The Orchestrator projects all declared stage refusal codes into the system-wide
// outcome registry (core/outcome) via Outcomes(). This enables compile-time and
// startup-time completeness checks across documentation, wire error mappers,
// and telemetry sinks without duplicating code lists.
package admission
