// Package admission provides the core statement admission pipeline vocabulary and orchestration.
//
// The admission pipeline is autodb's deterministic query inspection gate.
// Before any SQL statement is dispatched to a target database, it must be evaluated
// through an ordered chain of admission stages to detect dangerous patterns, verify
// transaction invariants, enforce predicate requirements, and block unauthorized GUC changes.
//
// ============================================================================
// ARCHITECTURE & COMPONENT FLOW
// ============================================================================
//
//	   +-------------------+       +--------------------+
//	   |       Facts       |       |      Context       |
//	   |  * Main Verb      |       |  * Transport Phys  |
//	   |  * Class (DML/DDL)|       |  * ReadOnly Floor  |
//	   |  * WHERE at D0    |       |  * TargetCaps      |
//	   |  * AST Mutations  |       |  * TxOpen / Pinned |
//	   +---------+---------+       +---------+----------+
//	             |                           |
//	             +-------------+-------------+
//	                           |
//	                           v
//	             +---------------------------+
//	             |     Orchestrator.Run      |
//	             +-------------+-------------+
//	                           |
//	          For each Stage in composed order:
//	          ┌────────────────┴────────────────┐
//	          │ Check Needs vs Context & Facts   │
//	          │   [Unsatisfiable? -> Skip]       │
//	          └────────────────┬────────────────┘
//	                           │ Applicable
//	                           v
//	          ┌─────────────────────────────────┐
//	          │       Stage.Apply(facts, ctx)   │
//	          └────────────────┬────────────────┘
//	                           │
//	              +------------+------------+
//	              |                         |
//	         (Operational)              (Policy)
//	           err != nil             Contribution
//	              |                         |
//	              v                         v
//	     [Abort Execution]          +-------+-------+
//	    *OperationalError           |               |
//	                           Deny != nil     Risk != nil
//	                                |               |
//	                                v               v
//	                         [HALT PIPELINE] [Collect Obs]
//	                         Returns Report   Continue loop
//	                         with PrimaryDeny
//
// ============================================================================
// CORE DESIGN PRINCIPLES
// ============================================================================
//
//  1. The Core Does Not Know What The Deny Logic Is
//     The execution engine asks, receives a Report, and if the Report denies, it
//     does not dispatch the statement. Everything that decides whether a statement
//     may run lives behind the Stage interface. The engine does not hardcode
//     denial rules.
//
//  2. Zero Protocol Wire Knowledge (Leaf Isolation)
//     This package is completely isolated from protocol wire formats (PostgreSQL
//     v3 wire protocol, error-code-to-SQLSTATE mappings, JSON-RPC envelopes).
//     The frontdoor proxy renders Reason into SQLSTATEs; the RPC server renders
//     into its own wire format. This isolation ensures admission logic remains
//     testable, deterministic, and embeddable anywhere.
//
//  3. Absence by Construction (Needs vs Context)
//     Stages declare what they require from Context and Facts via Needs. If a
//     target database lacks a capability (e.g. routine catalog), or if a statement
//     is not running in a session context, the stage is unsatisfiable and skipped
//     by construction—never silently returning empty contributions at runtime.
//
//  4. Two-Return Design: Policy vs Operational Failure
//     Stage.Apply returns (Contribution, error). These answer two fundamentally
//     different questions:
//       - Contribution: "Is there a policy violation or risk in this statement?"
//       - error: "Did the admission stage itself break (e.g., catalog read error)?"
//     Conflating policy rejection with infrastructure errors is strictly forbidden.
//
//  5. Mandatory Disclosure of Deny Codes
//     Every stage must explicitly declare all denial Codes it can emit via
//     DenyCodes(). If a stage denies with an undeclared code at runtime, the
//     Orchestrator immediately aborts with an OperationalError. This guarantees
//     that client renderers and audit collectors can discover all possible
//     rejection reasons from the live chain.
package admission

