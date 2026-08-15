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

**M0 — repo bootstrap.** Only `autodb --version` works. The milestone plan
(M0–M9) and per-milestone ADRs live in the project knowledge base
(kickoff record: ADR-0052).

## Layout

| Path | Role |
|---|---|
| `core/` | Package-of-record: config, meta-store, identity/authz/audit, execution, guards |
| `rpc/` | msgpack-RPC server + Go client (M5) |
| `tui/` | Standalone terminal UI on golib/tui (M6) |
| `lua/` | autovim/Neovim integration + binary lifecycle (M7) |
| `cmd/autodb/` | The single binary |

## Build

```sh
make build        # bin/autodb, version stamped from git describe
bin/autodb --version
```

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE).
