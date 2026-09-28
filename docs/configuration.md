# Configuration

autodb runs with no config at all. To change anything, copy the annotated
example — it ships every setting at its default value, so an uncommented copy
behaves exactly like no config:

```sh
mkdir -p ~/.config/autodb
curl -fsSL https://raw.githubusercontent.com/yongjohnlee80/autodb/main/config.example.toml -o ~/.config/autodb/config.toml
```

`--config <path>` overrides the location for `--serve`, `--ui` and `--web-ui`.
Unknown keys are **rejected rather than ignored**, and values are validated at
load — a bad port, bind, CIDR, or a PostgreSQL meta store without a DSN fails
before the server listens, naming the offending key.

## Who can reach the daemon

With no `[server] port` configured, the daemon listens on a unix socket whose
file is mode 0600 and owned by the OS user that started it. **The socket file
is the access control:** no other machine can reach it, and no other OS account
on the same machine can open it — not even to reach the login prompt. That
default is intended for **single-user** use: one person, one daemon, on their
own machine.

For a **multi-user** host — several people with their own SSH accounts and one
daemon serving them all — the preferred setup is to **map a port to the RPC
server**: set `[server] port` (the default `bind = "127.0.0.1"` keeps it on
loopback). Every OS account on that host can then SSH in, run `autodb --ui`,
log in with their own autodb credentials, and from the token manager mint their
Personal Access Token and whitelist the IP addresses it may be used from. Each
person is identified by their autodb login, not by their OS user; the bind stays
loopback, and exposing the port beyond the host is a separate, deliberate step
(see [`config.example.toml`](../config.example.toml)).

Two other routes exist and are second choices. The socket can be made
group-owned by hand (`chmod 660` on the live socket file, with the other users
in the daemon's group), but the daemon re-applies 0600 every time it binds, so
that must be redone after every restart — a footgun, not a configuration. And
`--web-ui` over an SSH port-forward reaches the same token manager from a
browser with no change to the daemon at all.

`autodb --print-endpoint` shows where a given config actually listens.

Verified on a shared host (Linux, two OS uids, one daemon): with the default
socket, the other uid's connect fails with `permission denied` while the owner
connects; after `chmod 660`, a member of the daemon's group connects and a
non-member is still refused; with `[server] port` set, the other uid connects
over `127.0.0.1` and the daemon serves it.

The meta store is autodb's own database — users, encrypted connection secrets,
grants, workspaces, audit log, script history — not one of the databases you
connect to. It runs on SQLite by default and on PostgreSQL for production
deployments (see [ops/postgres-meta-store.md](ops/postgres-meta-store.md)).
