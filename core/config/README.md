# Package `config` — Configuration Management & Validation

`config` loads, decodes, and validates autodb's TOML configuration. It provides zero-config defaults for standalone local developer environments while enforcing strict security validation for networked production deployments.

---

## 1. Architectural Overview

```
[autodb.toml File on Disk] OR [Absent File (Zero-Config Mode)]
                         │
                         ▼
             [config.Load / LoadFile]
                         │
        ┌────────────────┴────────────────┐
        ▼                                 ▼
[Decoder with Unknown-Key       [Zero-Config Fallback]
  Rejection: BurntSushi/toml]      Local Unix Domain Socket
        │                          Embedded SQLite Meta-Store
        ▼                          Conservative Sizing Limits
[Strict Semantic Validation]
  • Meta DSN sslmode=verify-full check
  • Unix socket path <= 100 bytes (sun_path)
  • Timeout and memory ceiling sanity checks
        │
        ▼
[Validated *config.Config Struct]
```

---

## 2. Configuration Sections Hierarchy

```
+------------------------------------------------------------------------+
|                                Config                                  |
+------------------------------------------------------------------------+
| [server]    IPC rendezvous, TCP port/bind, connection limits           |
| [meta]      Meta-store DSN, engine (sqlite/postgres), partition policy |
| [history]   Audit trail retention days, max history entries            |
| [security]  Master passphrase source, PBKDF2/Argon2 params, token TTL  |
| [tui]       Vim mode bindings, status line styling, query editor theme |
| [web]       Loopback HTTP/WebSocket UI port and CORS allowlists        |
| [exec]      Query timeouts, idle-in-tx limits, max statement bytes     |
| [frontdoor] PostgreSQL wire-protocol proxy listener, TLS certificates  |
+------------------------------------------------------------------------+
```

---

## 3. Core Design Principles & Security Hardening

### 3.1 Zero-Config by Default
If no configuration file exists at startup, autodb runs immediately without error. It automatically creates:
- A local unix domain socket in `$XDG_RUNTIME_DIR/autodb.sock` (mode `0700`).
- A local SQLite database in `$XDG_DATA_HOME/autodb/meta.db` (mode `0600`).
- Sensible memory, connection, and statement bounds.

### 3.2 Endpoint Rendezvous: Unix Socket vs TCP
- **Default Unix Socket**: Bound by default to `$XDG_RUNTIME_DIR/autodb.sock`. Unreachable across network boundaries; secured by OS filesystem file permissions.
- **Strict Path Length**: Verified against `maxSocketPath = 100` bytes to avoid silent kernel `sun_path` truncation (104 bytes on macOS, 108 bytes on Linux).
- **Explicit TCP Opt-in**: Setting a non-zero `server.port` explicitly enables TCP listening.

### 3.3 Meta DSN Hardening (`sslmode=verify-full`)
The meta-store stores encrypted connection credentials, audit logs, and user credentials.
- When configured against a remote PostgreSQL database, `sslmode=verify-full` is **mandatory**.
- `sslmode=require` is rejected because it does not authenticate the server identity (leaving the store vulnerable to MITM attacks).
- Insecure connections are permitted only if `meta.allow_insecure_transport = true` is explicitly specified.

---

## 4. Glossary of Terms

- **Provenance**: Tracking which configuration values were explicitly set by the operator in TOML versus supplied by autodb defaults.
- **Rendezvous**: The common socket endpoint where the server listens and all clients (TUI, Neovim) connect.
- **sun_path**: The C kernel struct field storing a Unix domain socket path.
- **`verify-full`**: The highest PostgreSQL SSL mode, verifying both the certificate CA and the host name.

---

## 5. Usage Example

### Go Loading Snippet
```go
package main

import (
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/config"
)

func main() {
    // Loads config from default paths or returns zero-config defaults
    cfg, err := config.Load()
    if err != nil {
        log.Fatalf("invalid configuration: %v", err)
    }

    endpoint, err := cfg.Server.Endpoint()
    if err != nil {
        log.Fatalf("invalid endpoint: %v", err)
    }

    fmt.Printf("autodb listening on %s (%s)\n", endpoint.Address, endpoint.Network)
}
```

### Example `autodb.toml`
```toml
[server]
port = 7419
bind = "127.0.0.1"

[meta]
engine = "postgres"
dsn = "postgres://autodb:secret@db.internal:5432/autodb_meta?sslmode=verify-full&sslrootcert=/etc/ssl/certs/ca.pem"

[security]
session_idle_timeout = "30m"
max_sessions_per_user = 8

[exec]
default_max_rows = 500
max_statement_bytes = 65536
idle_in_tx_timeout = "90s"
max_tx_duration = "5m"
```
