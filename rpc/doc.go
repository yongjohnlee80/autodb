// Package rpc implements the Msgpack-RPC management and query server for autodb.
//
// It serves as the primary control-plane interface consumed by the Neovim
// plugin, the terminal user interface (TUI), and external CLI automation.
//
// ============================================================================
// SUBSYSTEM ARCHITECTURE
// ============================================================================
//
// The package is designed as a strictly mechanical projection of core/auth and
// core/exec onto the binary Msgpack-RPC transport:
//
//	  1. Handshake & Version Gating (server.go):
//	     Connections are quarantined upon arrival. Only sys.hello is callable.
//	     The server enforces an exact protocol version match (Protocol = 7).
//	     Mismatched versions immediately poison the connection to signal client
//	     re-provisioning.
//
//	  2. Transport Context & Audit Threading (methods.go):
//	     Every request extracts the remote TCP or Unix peer address, injecting
//	     it directly into core/auth calls to enforce IP allowlists and guarantee
//	     audit trail fidelity.
//
//	  3. Stateful ExecSessions (methods.go):
//	     Provides transactional multi-statement query capabilities (session_open,
//	     session_run, session_close) with concurrency serialization and
//	     transaction state machine validation.
//
//	  4. Deny-Before-Disclose Error Policy (methods.go):
//	     Internal execution errors are withheld by default. Only errors
//	     explicitly listed in publicErrs are projected to the wire with stable
//	     negative integer error codes (e.g. CodeAuth, CodeDenied). All unmapped
//	     failures return an opaque generic internal error.
//
// ============================================================================
// PROTOCOL & PIPELINE FLOW
// ============================================================================
//
//	  [Incoming RPC Client]
//	            │
//	            ▼
//	    ┌──────────────┐
//	    │  sys.hello   │ ──► Enforces Protocol version (7) & admits session
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ Method Auth  │ ──► Validates in-band bearer token & extracts peer IP
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ Method Route │ ──► Dispatches to sys, auth, conn, exec, keyslot, or ca
//	    └──────┬───────┘
//	           │
//	           ▼
//	    ┌──────────────┐
//	    │ Error Mask   │ ──► Translates internal errors via publicErrs allowlist
//	    └──────────────┘
//
package rpc
