// Package frontdoor implements autodb's high-performance, secure PostgreSQL v3
// wire-protocol server.
//
// The front door allows standard PostgreSQL clients (psql, pgx, JDBC, ODBC,
// database GUIs) to connect directly to autodb as if it were a native PostgreSQL
// server. All incoming queries are classified, inspected by the unified statement
// admission pipeline, authorized against RBAC grants, and executed against backend
// database pools or dedicated stateful sessions.
//
// ============================================================================
// ARCHITECTURE & CONNECTION LIFECYCLE
// ============================================================================
//
//	   [Client (psql / pgx / JDBC)]
//	                 │
//	                 ▼
//	   [1. TCP Accept & Admission Barrier] (listener.go)
//	       • Sits behind acceptMu registration barrier for clean teardown.
//	       • Enforces concurrent connection budgets & per-source IP throttles.
//	                 │
//	                 ▼
//	   [2. TLS Negotiation] (startup.go, tls.go, certgen.go)
//	       • SSLRequest (packet code 80877103): replies 'S' (TLS) or 'N'.
//	       • Performs TLS handshake within TLSHandshakeDeadline (10s).
//	       • Validates ALPN and client certificates (mTLS) if configured.
//	                 │
//	                 ▼
//	   [3. Startup Packet & GUC Negotiation] (startup.go, params.go)
//	       • Decodes StartupMessage (PostgreSQL protocol 3.0).
//	       • Rejects cancel requests or protocol 2.0 requests.
//	       • Enforces PreAuthMaxBodyLen (64 KiB) and StartupDeadline (10s).
//	                 │
//	                 ▼
//	   [4. Authentication Exchange] (auth.go)
//	       • Dispatches authentication request within AuthDeadline (10s).
//	       • Verifies Personal Access Tokens (PAT), passwords, or mTLS identity.
//	       • Emits AuthenticationOk, ParameterStatus, BackendKeyData, and ReadyForQuery.
//	       • Re-arms socket deadline to IdleDeadline (30m).
//	                 │
//	                 ▼
//	   [5. Authenticated Session Loop] (session_loop.go, session_extended.go)
//	       • Dispatches Simple Query ('Q') and Extended Query ('P','B','D','E','S').
//	       • Enforces frame reading boundaries (frame_reader.go).
//	       • Enforces resident buffer memory lane budgets (general_lane.go).
//	                 │
//	                 ▼
//	   [6. Core Execution & Streaming] (query_seam.go, core/exec)
//	       • Routes admitted queries to core/exec.
//	       • Streams RowDescription, DataRow, and CommandComplete frames.
//	       • Emits timing-safe ErrorResponse on admission violation or execution failure.
//
// ============================================================================
// QUERY PROTOCOL LANES: SIMPLE VS EXTENDED
// ============================================================================
//
//	   SIMPLE QUERY PROTOCOL ('Q')
//	   ───────────────────────────
//	   Client:  Query ('Q') [SQL text]
//	   Server:  RowDescription ('T') -> DataRow ('D')* -> CommandComplete ('C') -> ReadyForQuery ('Z')
//	   * Atomic evaluation: parsed, admitted, and executed in a single cycle.
//
//	   EXTENDED QUERY PROTOCOL ('P' / 'B' / 'D' / 'E' / 'S')
//	   ──────────────────────────────────────────────────────
//	   1. Parse ('P'):
//	      • Tokenizes SQL and runs early static admission gates.
//	      • Prepares statement AST in session cache.
//	      • Emits ParseComplete ('1').
//	   2. Bind ('B'):
//	      • Binds positional parameters ($1, $2, ...) to statement.
//	      • Verifies parameter size and count bounds.
//	      • Emits BindComplete ('2').
//	   3. Describe ('D'):
//	      • Returns ParameterDescription ('t') or RowDescription ('T').
//	   4. Execute ('E'):
//	      • Evaluates late-binding admission gates (e.g. effective parameter values).
//	      • Runs statement against backend database.
//	      • Emits DataRow ('D')* and CommandComplete ('C').
//	   5. Sync ('S'):
//	      • Closes transaction boundaries and commits/rolls back.
//	      • Releases segment lane memory reservations.
//	      • Emits ReadyForQuery ('Z').
//
// ============================================================================
// RESOURCE MANAGEMENT & RESIDENT MEMORY LANES
// ============================================================================
//
// The front door operates in environments subject to memory bounds. To prevent
// untrusted clients from forcing high heap allocation via pipelined queries:
//
//	+--------------------------------------------------------------------------+
//	|                         Process-Wide Memory Cap                          |
//	+------------------------------------+-------------------------------------+
//	                                     |
//	               ┌─────────────────────┴─────────────────────┐
//	               ▼                                           ▼
//	   [General Lane (general_lane.go)]            [Segment Lane (session_loop.go)]
//	   • Bounds serialized pending output.         • Bounds active extended query
//	   • Backpressure applied if socket              pipelining buffers.
//	     write buffer fills up past watermark.     • Released on Sync ('S') or session
//	   • Protects against slow-drain clients.        abrupt disconnect.
//
// ============================================================================
// SECURITY & INVARIANTS
// ============================================================================
//
//   - Accept-Registration Barrier: All connections cross acceptMu before launch;
//     closing the listener guarantees that no orphaned session goroutines leak.
//   - Pre-Auth vs Post-Auth Limits: Anonymous peers are constrained to 64 KiB
//     messages and 10-second phase budgets; authenticated sessions transition to
//     64 MiB query limits and 30-minute idle timeouts.
//   - Timing-Safe Denials: Authentication and admission failures emit uniform
//     error frames with randomized timing delays to prevent side-channel leaks.
//   - Strict Framing Validation: The custom frame_reader sits ahead of pgproto3
//     to prevent buffer exhaustion and unread byte desynchronization.
package frontdoor
