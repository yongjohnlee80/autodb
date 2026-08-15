// Package core is autodb's package-of-record: configuration, the meta-store,
// identity/authorization/audit, SQL execution, and statement guards all live
// here. Every frontend (RPC server, TUI, Lua/autovim, gate-guard HTTP server)
// consumes this package - no business logic exists outside it (ADR-0052 §4).
//
// core is a library, not a service: the RPC and HTTP servers are thin
// consumers of it. Implementation lands across roadmap milestones M2-M4.
package core
