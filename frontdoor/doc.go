// Package frontdoor implements the PostgreSQL wire protocol frontend for autodb.
//
// It serves as the primary ingress gateway, managing client connections from
// initial TCP handshake through authentication, session execution, and clean teardown.
//
// ============================================================================
// SUBSYSTEM ARCHITECTURE
// ============================================================================
//
// frontdoor is organized around five core operational pillars:
//
//	  1. Linear Lifecycle Runner (runner.go, phase.go):
//	     Every connection progresses through an explicit sequence of phases:
//	     Accept -> Startup -> Cancel/AuthOpen -> Handshake -> Serve -> Cleanup.
//	     Undeclared outcomes or phase re-runs fail fast as internal faults.
//
//	  2. Dual Memory Lanes (general_lane.go):
//	     Resident memory is strictly isolated between two budgets:
//	     - Control Lane: Guaranteed 64 KiB per connection reserved at accept
//	       time to process capacity-releasing frames (Sync, Close, Terminate).
//	     - General Lane: Shared process-wide 1 GiB budget for in-flight query
//	       segments, serialized output, and retained objects.
//
//	  3. Extended Query Pipeline (session_extended.go, held_objects.go):
//	     Handles PostgreSQL extended query sub-protocol (Parse, Bind, Describe,
//	     Execute, Flush, Sync, Close) with two-stage memory reservation and
//	     segment message/byte limits.
//
//	  4. Demand-Driven Session Reclamation (demand_wake.go):
//	     Enables the backend scheduler to reclaim database connection leases from
//	     idle client sessions by issuing a wake knock via read deadline
//	     manipulation, preserving write availability for clean fatal error framing.
//
//	  5. Linear Resource Token (acceptToken.go):
//	     Guarantees strict LIFO release of socket handles, admission permits,
//	     active connection tracking, and wait group counters across all exit paths.
//
// ============================================================================
// PIPELINE & CONCURRENCY FLOW
// ============================================================================
//
//	  [Client Connection]
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ PhaseAccept  │ ──► Checks limits & IP throttle; issues acceptToken
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ PhaseStartup │ ──► Handles SSLRequest, TLS negotiation & StartupMessage
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │PhaseAuthOpen │ ──► Atomic credential verification & session allocation
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │PhaseHandshake│ ──► Sends AuthenticationOk, ParameterStatus & ReadyForQuery
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │  PhaseServe  │ ◄── Extended / Simple Query Loop (Control & General Lanes)
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ PhaseCleanup │ ──► LIFO discharge of acceptToken & socket termination
//	    └──────────────┘
//
package frontdoor
