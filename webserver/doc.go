// Package webserver implements autodb's HTTP and WebSocket web gateway, enabling
// browser-based access to the terminal user interface.
//
// Built upon golib/tui/web, the webserver bridges remote or desktop web browsers
// to autodb's interactive TUI without requiring a local terminal emulator or SSH
// session. The server streams optimized cell diffs over a binary WebSocket connection
// and translates browser keyboard and resize events into native TUI input.
//
// ============================================================================
// ARCHITECTURE & SESSION LIFECYCLE
// ============================================================================
//
//	   [Web Browser (Desktop / Tablet)]
//	                 │
//	                 ▼ (HTTP POST /login)
//	   [1. Authentication & Ticket Minting] (gateway.go)
//	       • Validates user credentials against autodb daemon via RPC.
//	       • Issues a short-lived, single-use Attach Ticket (TicketTTL = 30s).
//	                 │
//	                 ▼ (WebSocket /ws?ticket=...)
//	   [2. Ticket Redemption & WebSocket Upgrade] (gateway.go)
//	       • Redeems ticket; establishes full-duplex binary WebSocket stream.
//	       • Instantiates web-adapted TUI instance (golib/tui/web).
//	                 │
//	                 ▼
//	   [3. Per-User RPC Connection Pool] (sessions.go)
//	       • Attaches to shared per-user daemon session.
//	       • Increments reference count for the authenticated subject.
//	                 │
//	                 ▼
//	   [4. Interactive UI Streaming]
//	       • Server renders TUI frame cell diffs -> WebSocket -> Browser canvas.
//	       • Browser keyboard/mouse events -> WebSocket -> TUI event loop.
//	                 │
//	                 ▼
//	   [5. Detach & Disconnect Grace Period]
//	       • On tab close / network drop, session is marked detached.
//	       • Reconnection permitted within DefaultIdle (5 minutes).
//	       • If idle timer expires or all tabs close, session is reclaimed
//	         and auth.logout is invoked on the daemon.
//
// ============================================================================
// PER-USER CONNECTION POOLING
// ============================================================================
//
// To prevent browser tab proliferation from exhausting daemon connection limits,
// all browser sessions belonging to the same user share a single RPC session:
//
//	   Browser Tab 1 (User A) ───┐
//	                             ├──► [User A Pool Entry: RefCount=2] ──► 1 Daemon RPC Conn
//	   Browser Tab 2 (User A) ───┘
//
//	   Browser Tab 3 (User B) ──────► [User B Pool Entry: RefCount=1] ──► 1 Daemon RPC Conn
//
// ============================================================================
// SECURITY & ISOLATION INVARIANTS
// ============================================================================
//
//   - Zero Credential Persistence: The web client never stores master keys, DSNs,
//     or persistent passwords; all bearer tokens are managed server-side in memory.
//   - Single-Use Attach Tickets: WebSockets require cryptographic attach tickets
//     with a 30-second TTL, preventing cross-site WebSocket hijacking (CSWSH).
//   - Automatic Teardown & Logout: Reference counting guarantees that when a user's
//     final browser session terminates, their daemon session token is revoked.
//   - IP CIDR Allowlisting: Optional ipallow filters restrict gateway reachability
//     to authorized subnets.
package webserver
