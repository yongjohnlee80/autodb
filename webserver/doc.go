// Package webserver implements autodb's multi-user web interface gateway.
//
// It projects the terminal user interface (TUI) to web browsers over WebSockets
// using virtual ANSI terminal emulation, while terminating web authentication,
// pooling daemon connections per user, and enforcing concurrency limits.
//
// ============================================================================
// SUBSYSTEM ARCHITECTURE
// ============================================================================
//
//	  1. Preflight Daemon Probe (preflight.go):
//	     Ensures a compatible autodb daemon is actively running before binding
//	     the HTTP port. Fails fast if the daemon is absent or if a foreign
//	     occupant is detected.
//
//	  2. Per-User Pooled Sessions (sessions.go):
//	     Maintains a reference-counted pool of tuiapp.Session instances. Multiple
//	     browser tabs belonging to the same user share a single underlying RPC
//	     connection to the daemon. When the last tab disconnects and the idle
//	     timeout expires (DefaultIdle = 5m), the user is logged out.
//
//	  3. Two-Stage Ticket Authentication (gateway.go):
//	     Authenticates users via HTTP POST /login against the daemon's auth
//	     service, setting a secure cookie. Upgrading to the interactive WebSocket
//	     stream requires redeeming a short-lived, cryptographically signed ticket
//	     (TicketTTL = 30s) minted by GET /attach.
//
//	  4. Strict Loopback Posture (gateway.go):
//	     By default, listens strictly on 127.0.0.1. Remote administration is
//	     routed via SSH port forwarding or authenticated reverse proxies.
//
// ============================================================================
// PIPELINE & CONCURRENCY FLOW
// ============================================================================
//
//	  [Web Browser]
//	        │
//	        ▼
//	  ┌──────────────┐
//	  │ POST /login  │ ──► Authenticates against daemon & sets HTTP cookie
//	  └─────┬────────┘
//	        │
//	        ▼
//	  ┌──────────────┐
//	  │ GET /attach  │ ──► Mints short-lived attach ticket (TTL 30s)
//	  └─────┬────────┘
//	        │
//	        ▼
//	  ┌──────────────┐
//	  │ WS /connect  │ ──► Redeems ticket & joins per-user session pool
//	  └─────┬────────┘
//	        │
//	        ▼
//	  ┌──────────────┐
//	  │ Virtual Term │ ──► Bi-directional ANSI streaming via golib/tui/web
//	  └──────────────┘
//
package webserver
