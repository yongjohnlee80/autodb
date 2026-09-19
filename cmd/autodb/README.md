# cmd/autodb — Unified CLI Entry Point & Daemon Process

`cmd/autodb` compiles the primary `autodb` binary. It unifies server daemons,
terminal user interfaces, browser gateways, administrative bootstrap ceremonies,
TLS material generators, and database migration tooling into a single, cohesive
executable.

---

## 1. Architectural Overview & Execution Switchboard

The binary operates under a strict single-mode dispatch policy. Flags that govern
specific modes cannot be supplied outside their respective targets. Mutually
exclusive flags are rejected during pre-execution verification.

```
                             +-------------------+
                             |   autodb binary   |
                             +---------+---------+
                                       |
                   +-------------------+-------------------+
                   |           checkFlags()                |
                   |   (validates mutual exclusivity &     |
                   |    mode-specific parameters)          |
                   +-------------------+-------------------+
                                       |
    +-------------+-------------+------+------+-------------+-------------+
    |             |             |             |             |             |
    v             v             v             v             v             v
+-------+     +-------+     +-------+     +-------+     +-------+     +-------+
|--serve|     | --ui  |     |--web- |     |--init |     |--cre- |     |--mig- |
|Daemon |     | Term  |     |  ui   |     |First- |     |  ate- |     |  rate |
|RPC+PG |     |  TUI  |     |Web-UI |     |  Run  |     |  cert |     | -to-pg|
+---+---+     +---+---+     +---+---+     +---+---+     +---+---+     +---+---+
    |             |             |             |             |             |
    v             v             v             v             v             v
 [Server]     [Client]      [Gateway]     [Ceremony]    [PKI Gen]     [Catalog]
```

### Execution Mode Summary

| Mode Flag | Purpose | Primary Subsystem Dependencies |
|---|---|---|
| `--serve` | Runs the long-lived RPC server & PostgreSQL wire protocol front door | `rpc`, `frontdoor`, `core/exec`, `core/meta`, `core/auth` |
| `--ui` | Launches the interactive three-pane terminal user interface | `tui`, `golib/tui`, `rpc` (client session), `core/config` |
| `--web-ui` | Serves an in-browser projection of the TUI over WebSocket loopback | `webserver`, `tui`, `rpc` (client probe) |
| `--init` | First-run setup: creates root admin and wraps unattended keyslot | `core/auth`, `core/meta`, `core/config` |
| `--create-cert`| Generates internal CA and server certificates for TLS front door | `frontdoor`, `crypto/x509` |
| `--migrate-to-postgres` | Performs one-way catalog migration from SQLite to PostgreSQL | `core/meta`, `core/config` |
| `--print-endpoint` | Emits machine-readable `<network>\t<address>` for plugin discovery | `core/config` |
| `--version` | Prints version, git commit hash, and build timestamp | Build metadata (`-ldflags`) |

---

## 2. Daemon Architecture (`--serve`)

When invoked with `--serve`, the process bootstraps the full database engine,
acquires exclusive instance leases, unlocks storage keyslots, and starts
background maintenance tasks before accepting network connections.

```
+-----------------------------------------------------------------------------+
|                         autodb --serve Startup Flow                         |
+-----------------------------------------------------------------------------+
                                       |
                         [ Load & Validate Config ]
                                       |
                         [ Verify Non-Client Config ]
                          (Reject client_only = true)
                                       |
                       [ Bind IPC / TCP Listeners ]
                      (Probe existing occupant on EADDRINUSE)
                                       |
                          [ Pin Unix Socket Inode ]
                        (O_PATH file descriptor hold)
                                       |
                         [ Open Meta Store Catalog ]
                                       |
                       [ Acquire Exclusive Lease ]
                   (Advisory lock on PG / flock on SQLite)
                                       |
                     [ Initialize Authentication Service ]
                                       |
                    [ Unattended Service Keyslot Unlock ]
                   (If keyfile fails -> emit locked banner)
                                       |
                        [ Construct Execution Engine ]
                                       |
                      [ Load Durable Dynamic Policy ]
                                       |
                +----------------------+----------------------+
                |                                             |
                v                                             v
     [ Start Maintenance Loops ]                 [ Start Protocol Listeners ]
     - Janitor Timeout Sweeper                   - PostgreSQL Wire Front Door
     - Outcome Status Reconciler                 - msgpack-RPC Management Server
     - Partition Rolling Routine                              |
     - Audit Retention Scrubber                               |
                |                                             |
                +----------------------+----------------------+
                                       |
                          [ Serve Until Termination ]
                           (SIGINT / SIGTERM / Lost Lease)
                                       |
                          [ Graceful Drain & Unlink ]
```

### Key Daemon Operational Invariants

1. **Lease Fencing**: Only one daemon instance may serve a meta-store database. The
   lease heartbeat runs continuously. If the lease is lost, the daemon terminates
   immediately to prevent split-brain writes.
2. **Fail-Closed on Unattended Unlock**: If the service keyfile is missing or
   corrupted, the daemon does not crash. It enters a degraded mode: the process
   remains running to answer status requests, but all data queries are refused with
   a clear diagnostic banner until unlocked via administrator passphrase.
3. **Supervised Front Door**: If the PostgreSQL wire protocol front door encounters a
   fatal runtime error, the supervisor cancels the server context, refusing to run in
   a half-alive state where management RPC is up but wire access is down.

---

## 3. Unix Domain Socket Inode Pinning & Reclamation

When running over Unix domain sockets, standard filesystem unlinking on exit is
vulnerable to race conditions where an exiting process deletes the newly bound
socket of a successor instance.

`cmd/autodb` implements Linux `O_PATH` file descriptor holds to eliminate this hazard:

```
Predecessor Daemon                                     Successor Daemon
------------------                                     ----------------
1. Bind socket "/run/autodb.sock"
2. Open O_PATH fd on "/run/autodb.sock"
   (Pins inode X in kernel memory)
3. Serve clients...
                                                       4. Start new daemon instance
5. Predecessor begins shutdown                         5. Probe socket -> no answer
                                                       6. Unlink stale "/run/autodb.sock"
                                                       7. Bind new socket at same path
                                                          (Linux assigns new inode Y)
8. Run cleanup defer:
   - Compare pinned inode X with path inode Y
   - Inode X != Inode Y:
     Path belongs to successor!
   - DO NOT UNLINK!
   Successor continues running unharmed!
```

---

## 4. Frontend Launchers: `--ui` vs `--web-ui`

The CLI provides two user interface projections, each with distinct process
supervision semantics:

```
                            User Interface Modes
                            --------------------

           autodb --ui                                 autodb --web-ui
                |                                             |
     [ Probe Daemon Socket ]                       [ Probe Daemon Socket ]
                |                                             |
       +--------+--------+                                    |
       |                 |                                    |
   Answers?          No Answer?                           Answers?
       |                 |                                    |
       |          ClientOnly=false?                      +----+----+
       |                 |                               |         |
       |           +-----+-----+                        Yes        No
       |           |           |                         |         |
       |          Yes          No                        v         v
       |           |           |                      [Serve]   [Fail Fast]
       |           v           v                     (Browser   (Exit with
       |     [Spawn Daemon] [Refuse]                 Gateway)     Error)
       |     (Setsid Child)
       |           |
       v           v
    [Connect TUI Session]
```

- **Standalone TUI (`--ui`)**: Intended for local developers. If no daemon is
  answering and the configuration permits local execution (`client_only = false`),
  it automatically spawns a detached daemon child process (`Setsid: true`) redirecting
  output to `~/.local/state/autodb/serve.log`.
- **Browser Gateway (`--web-ui`)**: Intended for multi-user web hosting. It enforces
  a strict preflight check: it will never spawn a daemon. If no compatible daemon is
  running on the configured socket, it terminates immediately with exit code 1.

---

## 5. Subcommand & Flag Reference

### General Flags
- `--config <path>`: Explicit path to the configuration TOML file. If omitted, defaults
  to the user configuration directory (`~/.config/autodb/config.toml`) or system
  path (`/etc/autodb/config.toml`).
- `--version`: Prints binary version, commit SHA, and build timestamp, then exits.

### Daemon Server (`--serve`)
- `--serve`: Starts the background RPC server and PostgreSQL wire front door. Runs
  until interrupted by `SIGINT`, `SIGTERM`, or instance lease expiration.

### Terminal UI (`--ui`)
- `--ui`: Starts the interactive terminal interface attached to the active daemon.

### Web Projection Gateway (`--web-ui`)
- `--web-ui`: Binds a local HTTP/WebSocket server projecting the TUI to web browsers.
- `--port <1..65535>`: Port number for the web gateway (default: `7010`). Bound
  exclusively to `127.0.0.1`.

### First-Run Ceremony (`--init`)
- `--init`: Prompts for the initial administrator username and passphrase, creates
  the catalog schema, and wraps the master key into the configured service keyslot.

### TLS Certificate Tooling (`--create-cert`)
- `--create-cert`: Generates internal Certificate Authority and server certificates.
- `--cert-dir <path>`: Destination directory for generated keys and certificates
  (default: `tls/` beside the configuration file).
- `--cert-hosts <hosts>`: Comma-separated list of hostnames and IP addresses to include
  in the Subject Alternative Name (SAN) extension.
- `--leaf-only`: Reissues the server certificate using an existing CA private key.
- `--export-ca`: Emits `ca.pem` to stdout for client distribution and exits.
- `--force`: Overwrites existing CA files, invalidating previously issued certificates.

### Catalog Migration (`--migrate-to-postgres`)
- `--migrate-to-postgres`: Migrates all metadata from a SQLite catalog into PostgreSQL.
- `--from <path>`: Source SQLite database file path.
- `--to <dsn>`: Destination PostgreSQL connection string.
- `--dry-run`: Validates connectivity and schemas without committing data.
- `--allow-insecure-dsn`: Permits destination DSNs lacking `sslmode=verify-full`.

### Diagnostics & Discovery
- `--print-endpoint`: Prints the resolved daemon endpoint in `<network>\t<address>`
  format and exits.

---

## 6. Exit Codes

`cmd/autodb` uses standardized exit codes:

| Exit Code | Constant / Source | Meaning |
|---|---|---|
| `0` | Success / `EX_OK` | Clean process termination, or probe confirmed an already-running daemon |
| `1` | General Failure | Runtime error, failed preflight check, or abnormal daemon exit |
| `2` | Usage Error | Invalid command-line flags or mutually exclusive flag combinations |
| `78` | `exitConfig` (`EX_CONFIG`)| Configuration syntax error, unreadable config, or invalid semantic constraints |

---

## 7. Domain Jargon Glossary

- **Daemon Singleton**: The invariant ensuring that exactly one server process
  controls the database catalog and port bindings on a host.
- **Inode Pinning**: Holding an open Linux `O_PATH` file descriptor to a Unix domain
  socket file to ensure its inode cannot be recycled under an overlapping successor.
- **Unattended Unlock**: Automatic decryption of the master storage key on daemon
  startup using a protected local keyslot file, avoiding manual passphrase entry on boot.
- **Client-Only Configuration**: A configuration profile with `client_only = true`
  distributed to unprivileged users, stripped of database credentials and prohibited
  from starting daemons or initializing storage.
- **Foreign Host Hazard**: The condition where a user configuration on a service
  host attempts to bind production ports against a private catalog.
- **Instance Lease**: An active lock (`flock` on SQLite, advisory lock on PostgreSQL)
  held by the serving daemon to guarantee single-writer catalog safety.
- **Preflight Fast-Fail**: Rapid verification of daemon availability before
  allocating resources or opening network listeners in satellite frontends.
- **Supervised Front Door**: Active process monitoring ensuring that failure of
  the PostgreSQL wire listener causes the entire daemon to safely shut down.

---

## 8. Usage Examples

### Starting the Background Daemon
```bash
# Start daemon with default configuration
autodb --serve

# Start daemon with explicit configuration
autodb --serve --config /etc/autodb/config.toml
```

### Initializing a New Installation
```bash
# Run first-time setup ceremony as root
sudo autodb --config /etc/autodb/config.toml --init
```

### Launching User Interfaces
```bash
# Launch interactive terminal UI (spawns daemon if not running)
autodb --ui

# Launch browser gateway on port 8080
autodb --web-ui --port 8080
```

### Generating TLS Certificates for Front Door
```bash
# Generate CA and server certificates for local domain
autodb --create-cert --cert-hosts "db.internal,10.0.0.1"

# Export CA certificate for client distribution
autodb --create-cert --export-ca > /tmp/ca.pem
```

### Migrating SQLite Catalog to PostgreSQL
```bash
# Dry-run migration check
autodb --migrate-to-postgres \
  --from /var/lib/autodb/meta.db \
  --to "postgres://admin@db.example.com/autodb?sslmode=verify-full" \
  --dry-run

# Execute full transactional migration
autodb --migrate-to-postgres \
  --from /var/lib/autodb/meta.db \
  --to "postgres://admin@db.example.com/autodb?sslmode=verify-full"
```
