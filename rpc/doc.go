// Package rpc implements autodb's msgpack-RPC server over Unix domain sockets or loopback TCP.
//
// autodb exposes its administrative, authentication, schema inspection, and SQL execution
// capabilities through a structured msgpack-RPC surface built on golib's RPC foundation.
// Neovim connects natively via sockconnect("tcp", ..., {rpc = true}) or over a local Unix
// socket. The standalone TUI frontend consumes this exact same RPC boundary, even when running
// in-process, enforcing autodb's single-source-of-truth architectural rule by construction.
//
// ============================================================================
// ARCHITECTURAL ROLE & MECHANICAL PROJECTION
// ============================================================================
//
// The rpc package is strictly a mechanical projection layer:
//   - Zero Business Logic: The package contains no independent authorization decisions,
//     storage mutations, or query execution logic.
//   - Parameter Unpacking: Decodes positional msgpack arguments and validates bounds.
//   - Context Enrichment: Extracts the caller's peer network address (IP/socket) and threads
//     it through to core/auth and core/exec for audit logging and CIDR allowlist enforcement.
//   - Deliberate Error Projection: Translates internal core errors into structured, client-safe
//     wire error codes (deny-before-disclose principle).
//
// ============================================================================
// RPC CONNECTION & HANDSHAKE LIFECYCLE
// ============================================================================
//
//	   [Client Connect]
//	          │ (Unix Socket or Loopback TCP)
//	          ▼
//	   [Connection State: Unverified]
//	          │
//	          ├───────────────────────────────────────────────────────┐
//	          │                                                       │
//	   Method != sys.hello                                      Method == sys.hello
//	          │                                                       │
//	          ▼                                                       ▼
//	   [Refused: CodeHandshakeRequired]                        [Inspect Protocol Field]
//	                                                                  │
//	                    ┌─────────────────────────────────────────────┼───────────────────────────────┐
//	                    │                                             │                               │
//	            Protocol == rpc.Protocol                       Protocol Missing              Protocol != rpc.Protocol
//	                    │                                             │                               │
//	                    ▼                                             ▼                               ▼
//	          [Session Admitted]                             [Probe Answered]               [Session Poisoned]
//	     • Sets sessHello = true                        • Returns version & server     • Sets sessRefused = true
//	     • Unlocks full method surface                  • Leaves session unadmitted    • Audits protocol error
//	     • Ready for auth/query calls                   • Used by single-instance      • All future calls refused
//	                                                      guard / daemon auto-detect   • Client must re-provision
//
// ============================================================================
// REQUEST PROCESSING PIPELINE
// ============================================================================
//
//	   Inbound Msgpack Request Frame
//	                 │
//	                 ▼
//	   [Decode Limits Check] (msgpack.Limits: depth <= 16, str <= 1MB, total <= 2MB)
//	                 │
//	                 ▼
//	   [Handshake Gate] (Verify sessHello == true, reject if sessRefused)
//	                 │
//	                 ▼
//	   [Method Route & Unpack] (Positional arguments -> typed Go parameters)
//	                 │
//	                 ▼
//	   [Core Invocation] (core/auth, core/exec, core/meta with Peer IP context)
//	                 │
//	                 ├──────────────────────────────────────┐
//	                 │ Success                              │ Error
//	                 ▼                                      ▼
//	   [Payload Normalization]                [wireErr Error Classification]
//	     • Timestamps -> RFC3339Nano            • Structured error code (-32020..-32049)
//	     • Exotic driver types stringify        • Opaque masking for internal errors
//	                 │                                      │
//	                 └───────────────────┬──────────────────┘
//	                                     │
//	                                     ▼
//	                         Outbound Msgpack Response
//
// ============================================================================
// WIRE ERROR TAXONOMY
// ============================================================================
//
// Errors returned across the wire use structured JSON-RPC / msgpack-RPC numeric codes:
//   - CodeProtocolMismatch    (-32020): Client protocol does not match server version.
//   - CodeHandshakeRequired   (-32021): Request attempted before successful sys.hello.
//   - CodeAuth                (-32030): Bad credentials, expired session, locked store.
//   - CodeDenied              (-32031): Authorization refusal (role, connection grant, CIDR).
//   - CodeStatementRejected   (-32032): Admission rejection (unbounded mutation, size limit).
//   - CodeSessionNotFound     (-32040): ExecSession handle does not exist or expired.
//   - CodeSessionBusy         (-32041): Prior statement still executing on pinned session.
//   - CodeSessionCapExceeded  (-32042): Concurrency limit reached on active sessions.
//   - CodeTxState             (-32043): Statement conflicts with transaction state.
//   - CodeConnectionDraining  (-32044): Backend connection is undergoing drain/teardown.
//   - CodeNoSuchTx            (-32045): Transaction ID not found or expired.
//   - CodeInvalidToken        (-32046): PAT parameter error (duplicate name, invalid TTL).
//   - CodeKeyslot             (-32047): Service keyslot operation failure.
//   - CodeKeyslotUnavailable (-32048): Storage keyslot is unconfigured or locked.
//   - CodeKeyslotActive       (-32049): Operation cannot proceed while keyslot is active.
package rpc
