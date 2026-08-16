# autodb

A security-first DB-IDE backend: one static Go binary that is an
**msgpack-RPC server** (Neovim-native over TCP), a **standalone TUI**
(lazysql-class, neovim keybindings), and — via the bundled Lua integration —
the backend of [autovim](https://github.com/yongjohnlee80/autovim)'s `dbase`
section. Built on [golib](https://github.com/yongjohnlee80/golib)
(`dao`, `tui`, `logger`).

Every frontend goes through one core: users (`admin | editor | reader`),
per-connection grants, token sessions, IP whitelisting, encrypted-at-rest
connection credentials, full audit of every executed statement, and
guardrails such as blocking `UPDATE`/`DELETE` without a `WHERE` clause.
Postgres, MySQL, and SQLite first.

## Status

**M5 — the RPC server is live.** `autodb --serve` runs the msgpack-RPC
server over the full core: config + meta-store (M2), identity/authz/audit
(M3), the SQL execution engine (M4), and the handshake-gated method surface
with the shared-server lifecycle (M5). The TUI (M6) and the autovim Lua
integration (M7) are next. The milestone plan (M0–M9) and per-milestone
ADRs live in the project knowledge base (kickoff record: ADR-0052).

## Layout

| Path | Role |
|---|---|
| `core/` | Package-of-record: config, meta-store, identity/authz/audit, execution, guards |
| `rpc/` | msgpack-RPC server ([README](rpc/README.md)); the Go client lands with the TUI (M6) |
| `tui/` | Standalone terminal UI on golib/tui (M6) |
| `lua/` | autovim/Neovim integration + binary lifecycle (M7) |
| `cmd/autodb/` | The single binary |

## Build & run

```sh
make build        # bin/autodb, version stamped from git describe
bin/autodb --version
bin/autodb --serve            # msgpack-RPC on 127.0.0.1:7419 (config-overridable)
```

`--serve` binds loopback by default, drains gracefully on SIGINT/SIGTERM,
and implements the single-instance guard: if the port is taken by a
compatible autodb it reports "already running" and exits 0; a foreign
occupant is a loud error. Protocol handshake, method surface, and error
codes are documented in [rpc/README.md](rpc/README.md).

In a bare-repo + worktree dev checkout, use the make targets (or pass
`-buildvcs=false`): Go's nested-VCS detection resolves to the bare store and
plain `go build` fails with "error obtaining VCS status".

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE).
