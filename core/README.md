# core

autodb's package-of-record. Configuration, meta-store, identity /
authorization / audit, SQL execution, and statement guards. Every frontend
(RPC, TUI, Lua, gate-guard HTTP) consumes this package; no business logic
exists outside it.

Implementation lands across roadmap milestones M2 (meta-store & config),
M3 (identity/authz/audit), and M4 (execution engine).

Licensed under [Apache-2.0](../LICENSE).
