# cmd/autodb

The primary executable entry point for `autodb`. The binary unifies the background RPC server daemon, the PostgreSQL wire-protocol frontdoor, the interactive terminal UI, the web gateway, and administrative maintenance utilities into a single compiled Go artifact.

---

## 1. Operational Modes & Flag Dispatch

The command-line flags select mutually exclusive execution modes:

| Flag | Category | Purpose |
| :--- | :--- | :--- |
| `--serve` | Daemon | Starts the background RPC server and the PostgreSQL front door listener. |
| `--ui` | Client | Launches the standalone terminal user interface, connecting to an existing daemon. |
| `--web-ui` | Gateway | Starts the HTTP and WebSocket web gateway, serving the TUI to web browsers. |
| `--port` | Gateway | Port for `--web-ui` (default: `8080`, bound to `127.0.0.1`). |
| `--init` | Admin | First-run ceremony: creates root administrator credentials and enrols keyslots. |
| `--create-cert` | Security | Generates CA and TLS certificates for the frontdoor listener. |
| `--migrate-to-postgres` | Migration | Migrates metadata from a SQLite database file into PostgreSQL. |
| `--print-endpoint` | Scripting | Prints the resolved daemon endpoint (`<network>\t<address>`) for client discovery. |
| `--version` | Telemetry | Prints compiled semver, commit SHA, and build timestamp. |

*(Note: Running `autodb` without flags launches the in-process development mode, running an embedded daemon and TUI in the same process).*

---

## 2. Subsystem Startup Pipeline (`--serve`)

```
                          [autodb --serve]
                                 │
                                 ▼
                     [1. Load Configuration]
                     • Resolves from --config or XDG directories
                     • Validates strict TOML sections
                                 │
                                 ▼
                 [2. Initialize Meta-Store & Keyslots]
                 • Mounts SQLite or PostgreSQL meta store
                 • Loads encrypted master keyslot
                                 │
                                 ▼
                    [3. Assemble Core Services]
                    • Boots core/auth service & RBAC
                    • Configures statement admission pipeline
                    • Initializes core/exec connection pool
                                 │
                 ┌───────────────┴───────────────┐
                 ▼                               ▼
       [4. Start RPC Server]          [5. Start PostgreSQL Front Door]
       • Binds Unix socket or TCP     • Binds TCP 5432 (default)
       • Serves Neovim & TUI clients  • Serves psql & database drivers
                 │                               │
                 └───────────────┬───────────────┘
                                 │
                                 ▼
                 [6. Signal Handler: SIGINT / SIGTERM]
                 • Gracefully drains active sessions
                 • Flushes audit logs & drops socket files
```

---

## 3. First-Run Ceremony (`--init`)

The `--init` flag performs the first-run provisioning ceremony required before starting the daemon as an unattended system service:
1. Validates that the meta-store has been initialized.
2. Prompts for (or reads from automation) the initial administrator username and passphrase.
3. Enrols the master encryption key into the unattended unlock keyslot.
4. Exits cleanly, allowing `systemd` or supervisors to launch `autodb --serve`.

```bash
autodb --init --config /etc/autodb/config.toml
```

---

## 4. Certificate Authority & TLS Generation (`--create-cert`)

To ensure frontdoor connections can be encrypted immediately without third-party certificate infrastructure, `autodb` includes a built-in zero-config CA and leaf certificate generator:

```bash
# Generate CA and server certificate in default directory
autodb --create-cert

# Generate certificate with custom SAN hostnames and export CA
autodb --create-cert --cert-hosts "db.internal,10.0.0.5" --export-ca > ca.pem

# Reissue server certificate from existing CA without invalidating clients
autodb --create-cert --leaf-only
```

---

## 5. Metadata Migration (`--migrate-to-postgres`)

Enables moving from a single-node SQLite deployment to a production PostgreSQL metadata store:
- Copies connections, users, roles, IP allowlists, and keyslot entries atomically.
- Re-encrypts connection secrets using the target database encryption context.
- Refuses to overwrite non-empty destination databases to prevent accidental data destruction.

```bash
autodb --migrate-to-postgres \
  --from /var/lib/autodb/meta.db \
  --to "postgres://admin:pass@postgres.internal:5432/autodb_meta?sslmode=verify-full" \
  --dry-run
```
