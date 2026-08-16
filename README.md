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

**M6 — the standalone TUI is live.** `autodb --ui` is a working DB IDE:
a three-pane layout (explorer / query editor / results), a vim-modal
editor, lazy schema browsing, table and JSON results, connection,
workspace and user management, per-workspace SQL notes, script history,
and in-panel search. `autodb --serve` runs the msgpack-RPC server over
the same core: config + meta-store (M2), identity/authz/audit (M3), the
SQL execution engine (M4), and the handshake-gated method surface with
the shared-server lifecycle (M5).

Next is **M7**, the autovim Lua integration. The milestone plan (M0–M9)
and per-milestone ADRs live in the project knowledge base (kickoff
record: ADR-0052; TUI architecture: ADR-0057).

## The TUI

```sh
bin/autodb --ui
```

First run walks you through creating the root user and the master
passphrase. Everything else hangs off the leader key:

| Key | Does |
|---|---|
| `Space` | leader menu — every command, with its binding |
| `?` | the keys available RIGHT HERE (focused panel, or the open modal) |
| `Ctrl-h/j/k/l` | move between panes (vim window motions) |
| `SPC r` / `SPC R` | run the buffer / run the selection |
| `SPC C` | choose which connection the query runs against |
| `SPC c` `SPC w` `SPC u` | connections, workspaces, users |
| `SPC H` | script history — who ran what, when, against which connection |
| `SPC n` `SPC s` | new note / save note (per-workspace `.sql` files) |
| `/` `n` `N` | search the focused panel, next/previous match |
| `SPC z` / `Ctrl-w z` | zoom the focused pane |
| `SPC x` / `SPC X` | disconnect-reconnect / restart the backend (admin) |
| `SPC A` | about: build, backend, and where state lives |
| `Ctrl-q` | quit (the shared server keeps running) |

The editor is vim-modal (`jk` escapes); the explorer and results honour
`j/k/g/G`; the JSON results view and the script viewer are read-only vim
buffers — navigate, select and yank, never edit.

**The server outlives the TUI by design** (one shared server, many
frontends), so a rebuilt binary keeps talking to the process already
running. `SPC X` restarts it from inside the UI; a protocol mismatch
says which side is stale.

## Layout

| Path | Role |
|---|---|
| `core/` | Package-of-record: config, meta-store, identity/authz/audit, execution, guards |
| `rpc/` | msgpack-RPC server ([README](rpc/README.md)); the Go client lands with the TUI (M6) |
| `tui/` | Standalone terminal UI on golib/tui (M6) |
| `lua/` | autovim/Neovim integration + binary lifecycle (M7) |
| `cmd/autodb/` | The single binary |
| `config.example.toml` | Every setting, with its default and why it is that |

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

## Configuration

autodb runs with no config at all: a first run needs no setup. The
defaults are loopback-only RPC on `127.0.0.1:7419` and a sqlite meta
store at `$XDG_DATA_HOME/autodb/meta.db`.

To change any of it, copy the annotated example — it ships every
setting at its default value, so an uncommented copy behaves exactly
like no config:

```sh
mkdir -p ~/.config/autodb
cp config.example.toml ~/.config/autodb/config.toml
```

`--config <path>` overrides the location for both `--serve` and `--ui`
(the TUI passes it to the server it spawns). Unknown keys are rejected
rather than ignored, and values are validated at load — a bad port,
bind, CIDR, or a postgres meta store without a DSN fails before the
server listens, naming the offending key.

The meta store is autodb's own database — users, encrypted connection
secrets, grants, workspaces, audit log, script history — not one of the
databases you connect to. Treat it as a credential store: the encrypted
DSNs cannot be recovered without it.

In a bare-repo + worktree dev checkout, use the make targets (or pass
`-buildvcs=false`): Go's nested-VCS detection resolves to the bare store and
plain `go build` fails with "error obtaining VCS status".

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE).
