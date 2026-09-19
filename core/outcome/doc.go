// Package outcome is the canonical authority for system outcomes, failure classification,
// throttle attribution, and runtime authorization witnesses.
//
// It unifies previously disconnected vocabularies across admission, frontdoor, and
// engine into an immutable, conflict-free registry.
//
// ============================================================================
// THE ORTHOGONAL DECOUPLING TRIAD
// ============================================================================
//
// outcome enforces complete independence across three core questions:
//
//	  1. Kind: What sort of ending occurred?
//	     • Refusal: Explicit decision not to proceed.
//	     • Control: Protocol lifecycle action (e.g. cancel request).
//	     • Operational: Error-driven ending (e.g. broken socket, store timeout).
//	     • Note: Non-fatal diagnostic observation.
//
//	  2. Charge: Who is answerable?
//	     • Credential: Peer presented bad secrets (charges throttle).
//	     • Protocol: Peer spoke invalid protocol/handshake (charges throttle).
//	     • Capacity: System ran out of resources (never charged).
//	     • None: Internal error / stored state (never charged).
//	     • NotApplicable: Outcome cannot reach throttle (e.g. statement admission).
//
//	  3. Wire Projection: What is the peer told?
//	     • Determined dynamically by the renderer, keyed on the Authorization Witness.
//
// ============================================================================
// THE AUTHORIZATION WITNESS
// ============================================================================
//
// A static reason-to-wire-code table leaks capacity telemetry to unauthorized probes.
// For instance, capacity exhaustion must report SQLSTATE 28000 (invalid authorization)
// to strangers, but SQLSTATE 53300 (too many connections) to authenticated operators:
//
//	                    [Capacity Refusal Occurs]
//	                                │
//	                   o.Disclosable (Witness)?
//	                                │
//	                 YES ───────────┴─────────── NO
//	                  │                           │
//	                  ▼                           ▼
//	           [SQLSTATE 53300]            [SQLSTATE 28000]
//	           (Authenticated Client)      (Anonymous Stranger)
//
// The Disclosable witness is attached explicitly at the raise site via Authorized()
// when verified credentials and grants are already in hand. It is NEVER derived
// from the reason string.
//
// ============================================================================
// COMPOSITION & VALIDATION CONTRACTS
// ============================================================================
//
//   - Zero Default Failure: KindUnset and ChargeUnset fail fast during Compose.
//   - Multi-Producer Agreement: Multiple producers may declare the same ReasonID,
//     but their Kind and Charge declarations must match identically.
//   - Raise Site Verification: Occur refuses any ReasonID that was not declared,
//     or that was not declared by the specific calling ProducerID.
package outcome
