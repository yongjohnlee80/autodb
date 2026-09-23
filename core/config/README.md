# core/config

`core/config` is `autodb`'s package-of-record for configuration management. It governs the parsing, validation, derivation, and security hardening of runtime options for the background daemon, the interactive terminal UI, the PostgreSQL wire frontdoor, and operational tools.

The configuration file is entirely optional: an absent configuration file yields deterministic, zero-config local defaults. When a file is supplied, it is decoded with strict unknown-key rejection and validated comprehensively at startup. Misconfiguration fails fast during `Load` rather than deferred until first connection or active query execution.

---

## Architecture & Visual Diagrams

### 1. Configuration Discovery & Search Precedence

When `autodb` starts without an explicit `--config` flag, it resolves the configuration file by inspecting candidate paths in order. A file must be both present and readable by the current process to be selected.

```
                    [ResolvePath(path)]
                             │
               path != ""? ──┴── path == ""
                   │                  │
                   ▼                  ▼
             [Return Path]      [DefaultPath()]
                                      │
                                      ▼
             ┌──────────────────────────────────────────────────┐
             │ 1. System Server Config:                         │
             │    /etc/autodb/config.toml (Mode 0640)           │
             │    • Owned by root / autodb service account      │
             │    • Complete configuration including meta DSN   │
             └────────────────────────┬─────────────────────────┘
                                      │ Present & Readable?
                            YES ──────┴────── NO
                             │                │
                             ▼                ▼
                        [Return Path]   ┌──────────────────────────────────────────────────┐
                                        │ 2. User Workspace Config:                        │
                                        │    $XDG_CONFIG_HOME/autodb/config.toml           │
                                        │    (~/.config/autodb/config.toml)                │
                                        │    • Per-developer explicit customizations       │
                                        └─────────────────────┬────────────────────────────┘
                                                              │ Present & Readable?
                                                    YES ──────┴────── NO
                                                     │                │
                                                     ▼                ▼
                                                [Return Path]   ┌──────────────────────────────────────────────────┐
                                                                │ 3. System Client Config:                         │
                                                                │    /etc/autodb/client.toml (Mode 0644)           │
                                                                │    • World-readable endpoint pointer             │
                                                                │    • Sanitized: omits credentials & meta secrets │
                                                                └─────────────────────┬────────────────────────────┘
                                                                                      │ Present & Readable?
                                                                            YES ──────┴────── NO
                                                                             │                │
                                                                             ▼                ▼
                                                                        [Return Path]   [Return User Path (Defaults apply)]
```

#### Service Host Detection & Spoofing Guard

On a shared server hosting `autodb` as a system daemon, an unprivileged user running the TUI must not inadvertently spawn a duplicate background daemon on the service's port:

```
                  os.Stat("/etc/autodb/config.toml")
                                  │
                    File exists? ─┴─ File missing?
                         │                 │
                         ▼                 ▼
             [ServiceHostSeen = true]    [ServiceHostSeen = false]
                         │                         │
                         │                         ▼
                         │               (Standard Desktop / Laptop)
                         │               • Frontend auto-spawns daemon
                         │                 if nothing is listening
                         ▼
             Does /etc/autodb/config.toml EXIST on this host?
                         │
             YES ────────┴──────── NO
              │                    │
              ▼                    ▼
   [ServiceHostSeen]        [Single-user install]
   • NO frontend spawns     • The first frontend to find
     a daemon -- systemd      nothing listening brings the
     owns that job            daemon up
   • Applies to EVERY
     config, the service's
     own included: holding
     it does not make a
     frontend the daemon
   • (`--serve` itself is
     unaffected; it never
     consults this)
```

---

### 2. Frontdoor Sizing & Headroom Derivation

The PostgreSQL wire listener (`[frontdoor]`) shares target database connection pools with internal control queries and interactive TUI sessions. Sizing parameters are derived to guarantee that frontdoor wire leases never starve interactive operators:

```
  [Host Hardware]
         │
         ▼
  [Exec.PoolMaxConns] ──────────────────────┐
  • Default: 2 × CPU cores                  │
  • Bounds per-target connections           │
                                            ▼
                             [DefaultReservedHeadroom(pool)]
                             • Calculated as: min(4, pool / 2)
                             • Holds connections back for TUI / control queries
                             • Never inflates the pool to satisfy reservation
                                            │
                                            ▼
                             [FrontDoor.EffectiveMaxLeases]
                             • Derived: PoolMaxConns - ReservedHeadroom
                             • Explicit max_leases may only be lower
                                            │
                                            ▼
                             [Capacity Validation]
                             • PoolMaxConns - ReservedHeadroom >= 1
                             • Refuses configurations leaving 0 capacity
```

#### Memory Budget Hierarchy

```
  ┌────────────────────────────────────────────────────────────────────────┐
  │                           Host Memory Ceilings                         │
  └───────────────────┬────────────────────────────────┬───────────────────┘
                      │                                │
                      ▼                                ▼
         ┌─────────────────────────┐      ┌─────────────────────────┐
         │   ResidentBudgetBytes   │      │    GeneralLaneBytes     │
         │   (Default: 1 GiB)      │      │    (Default: 1 GiB)     │
         │   (Ceiling: 4 GiB)      │      │    (Ceiling: 4 GiB)     │
         └────────────┬────────────┘      └────────────┬────────────┘
                      │                                │
                      ▼                                ▼
         Bounds fixed per-session         Bounds input segments, portals,
         memory reservations across       and serialized statement output
         all active wire sessions.        across all active wire sessions.
```

---

### 3. Meta-Store DSN Security & Transport Hardening

The meta store holds user identities, audit journals, and encrypted connection secrets. Connecting over an insecure network link without certificate verification is strictly prohibited:

```
                       [Incoming Meta DSN]
                                │
                                ▼
                       [checkMetaDSNTransport]
                                │
                  Engine == Postgres && DSN != ""?
                                │
                    YES ────────┴──────── NO ──► [Allow SQLite]
                     │
                     ▼
             Extract sslmode parameter
                     │
     ┌───────────────┼───────────────┬──────────────────────────────┐
     │               │               │                              │
     ▼               ▼               ▼                              ▼
[sslmode=disable] [sslmode=allow/  [sslmode=require/            [sslmode=verify-full]
                  prefer]          verify-ca]                       │
     │               │               │                              ▼
     │               │               │                     Has sslrootcert?
     ▼               ▼               ▼                              │
┌────────────────────────────────────────────────────────┐     YES ─┴─ NO
│ Refused: Does not authenticate server identity.        │      │       │
│ Vulnerable to active MITM credential theft.            │      │       ▼
│                                                        │      │   [Refuse: Unpinned CA]
│ (Overridden ONLY if allow_insecure_dsn = true is set)  │      ▼
└────────────────────────────────────────────────────────┘  [Safe: Admitted]
```

#### Pool Floor Enforcement

```
                       [EffectivePoolMaxConns]
                                │
        1. Explicit [meta] pool_max_conns
        2. DSN parameter: ?pool_max_conns=N
        3. Built-in default: 8
                                │
                                ▼
                       [checkMetaPoolFloor]
                                │
                 Effective Pool Bound < 2?
                                │
                    YES ────────┴──────── NO
                     │                     │
                     ▼                     ▼
             [Refuse Config]         [Admit Pool]
             • Instance lease pins
               1 permanent connection
             • Minimum 2 connections
               required to prevent
               deadlock during migrations
```

---

### 4. Local & Remote Endpoint Resolution

`autodb` communicates via MessagePack-RPC. The server listens on a local Unix domain socket by default, opting into TCP only when a port is explicitly defined:

```
                         [Server.Endpoint()]
                                  │
                         s.Port > 0 (Explicit)?
                                  │
                    YES ──────────┴────────── NO
                     │                         │
                     ▼                         ▼
             [Network: "tcp"]          [Network: "unix"]
             Address: host:port        Resolve socket directory:
             (Uses net.JoinHostPort)   1. Explicit server.socket
                                       2. Linux: $XDG_RUNTIME_DIR
                                       3. macOS: os.TempDir() (/var/folders)
                                       4. Fallback: $XDG_STATE_HOME/autodb
                                               │
                                               ▼
                                    Check kernel sun_path bound:
                                    len(path) <= 100 bytes
                                    (Guards against Darwin 104 / Linux 108 limit)
```

---

## Jargon & Domain Terminology

| Term | Definition |
| :--- | :--- |
| **Provenance** | Tracking whether a configuration value was explicitly provided by the operator or supplied by internal defaults. Recorded via decoder key tracking (`seen` map) rather than value comparisons. |
| **Reserved Headroom** | The number of connection slots in a target pool reserved exclusively for TUI operators and background control queries, withheld from wire clients. |
| **Wire Lease** | A target database connection allocated to an external PostgreSQL frontdoor client session. |
| **Service Host** | A system where `autodb` is installed as a system-level background daemon (`/etc/autodb/config.toml` exists). |
| **Foreign Config** | A user-level configuration file executed on a Service Host. Forbidden from starting a daemon to prevent port and credential collisions. |
| **Unattended Unlock** | Automatic decryption of the master keyslot on daemon launch via a restricted keyfile (`service_keyfile`), requiring no human interactive passphrase entry. |
| **Control Lane** | Memory buffer reserved for PostgreSQL wire protocol frame headers and out-of-band cancellation signals (`max_conns × 64 KiB`). |
| **General Lane** | Shared memory budget allocating memory for query text, statement descriptors, and buffered result sets. |

---

## Configuration Schema Reference

Below is the complete reference of configuration options supported in `config.toml`.

### `[server]`

Controls the MessagePack-RPC server listener.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `port` | integer | `0` | TCP port to listen on. Setting `0` opts into a Unix domain socket. |
| `bind` | string | `"127.0.0.1"` | IP address to bind the TCP listener to when `port > 0`. |
| `socket` | string | `""` | Absolute path for the Unix domain socket. Empty resolves via platform runtime dir. |
| `client_only` | boolean | `false` | When `true`, this configuration is forbidden from launching a daemon. |

### `[meta]`

Configures `autodb`'s internal metadata and authorization store.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `engine` | string | `"sqlite"` | Meta-store engine backend: `"sqlite"` or `"postgres"`. |
| `path` | string | `""` | File path for SQLite database. Empty defaults to `$XDG_DATA_HOME/autodb/meta.db`. |
| `dsn` | string | `""` | PostgreSQL connection string. Required when `engine = "postgres"`. |
| `allow_insecure_dsn` | boolean | `false` | When `true`, permits PostgreSQL DSNs without `sslmode=verify-full`. |
| `pool_max_conns` | integer | `8` | Maximum connections for the meta store. Must be at least `2`. |

### `[history]`

Configures statement execution history persistence.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | boolean | `true` | Enables recording executed SQL statements to user history. |

### `[security]`

Configures network admission and master keyslot unlocking.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `ip_allowlist` | string list | `["127.0.0.1/32", "::1/128"]` | CIDR blocks permitted to connect to the RPC server. |
| `service_keyfile` | string | `""` | Path to keyfile for unattended unlocking. Must have mode `0600` or `0400`. |

### `[tui]`

Configures interactive terminal user interface settings.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `notes_dir` | string | `""` | Directory for scratchpad notes. Defaults to `$XDG_DATA_HOME/autodb/notes`. |

### `[exec]`

Configures the SQL execution engine and connection pool boundaries.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `max_statement_bytes` | integer | `65536` | Maximum SQL statement size permitted for execution (64 KiB). |
| `max_target_conns` | integer | `0` (None) | Global ceiling for target connections. Required when frontdoor is enabled. |
| `max_sessions_per_user`| integer | `8` | Maximum concurrent execution sessions per user identity. |
| `max_sessions_global` | integer | `256` | Maximum concurrent execution sessions across all users. |
| `session_idle_timeout` | duration | `"10m"` | Inactivity duration before an idle execution session is reaped. |
| `idle_in_tx_timeout` | duration | `"2h"` | Maximum time an open transaction may remain idle between statements. |
| `max_tx_duration` | duration | `"8h"` | Maximum total lifetime of any open transaction before automated rollback. |
| `debug_idle_in_tx_timeout`| duration | `"2h"` | Deprecated transaction idle timeout for debug connections. |
| `max_tx_duration_ceiling` | duration | `"8h"` | Upper bound for per-connection transaction duration overrides. |
| `pool_max_conns` | integer | `2 × NumCPU` | Maximum physical database connections per target pool. |
| `pool_max_conn_idle_time`| duration | `"10m"` | Time before an unused pooled connection is closed. |
| `pool_max_conn_lifetime` | duration | `"60m"` | Maximum duration a pooled connection is kept alive. |
| `janitor_interval` | duration | `"10s"` | Frequency of sweeps reaping expired transactions and abandoned sessions. |
| `reconcile_interval` | duration | `"1m"` | Frequency of background outcome checks against down target databases. |
| `outcome_retention` | duration | `0` (Off) | Duration settled transactions are kept before collapsing to tombstones. |
| `outcome_retention_interval`| duration | `"1h"`| Frequency of outcome retention compaction passes. |

### `[frontdoor]`

Configures the PostgreSQL wire-protocol server.

| Key | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | boolean | `false` | Enables the PostgreSQL wire-protocol listener. |
| `bind` | string | `"127.0.0.1:5432"` | TCP host and port to bind the wire listener. |
| `tls_cert_file` | string | `""` | Path to server X.509 certificate file (PEM format). |
| `tls_key_file` | string | `""` | Path to server TLS private key file (PEM format). |
| `tls_host_names` | string list | `[]` | Hostnames expected in client connections; verified against certificate SANs. |
| `tls_root_ca_file` | string | `""` | Optional CA certificate file used to verify custom internal certificates. |
| `reserved_headroom` | integer | `min(4, pool/2)` | Connections withheld from wire clients for internal/interactive use. |
| `max_leases` | integer | `Derived` | Maximum concurrent target connections leased to wire clients. |
| `resident_budget_bytes`| integer | `1073741824` | Total memory reserved for active wire session tracking (Default: 1 GiB). |
| `general_lane_bytes` | integer | `1073741824` | Total memory allocated for queries, portals, and wire output buffers. |
| `control_lane_bytes` | integer | `Derived` | Memory allocated for wire frame handling (`max_conns × 64 KiB`). |
| `max_conns` | integer | `320` | Maximum simultaneous client network connections. |
| `pre_auth_conns` | integer | `64` | Maximum connections allowed in the pre-authentication stage. |
| `auth_workers` | integer | `16` | Maximum concurrent cryptographic password verification workers. |
| `auth_failures_per_ip` | integer | `10` | Rate-limiting threshold for failed login attempts per source IP. |
| `insecure_disable_tls` | string | `""` | Disables TLS. Must equal `"i-accept-that-every-pat-crosses-in-cleartext"`. |

---

## Architectural Decisions & Security Invariants

1. **No Default for `max_target_conns`**:
   Target database limits cannot be probed safely from the client side. Defaulting to an arbitrary number risks overrunning database capacity. The operator must specify this limit explicitly when enabling the frontdoor.
2. **Explicit Cleartext Acknowledgment**:
   Setting `insecure_disable_tls` requires typing the exact sentence `"i-accept-that-every-pat-crosses-in-cleartext"`. This prevents accidental deployment of cleartext authentication where access tokens could be sniffed.
3. **Decoupled Structural Interface (`core/meta.StoreConfig`)**:
   `core/meta` requires configuration parameters (`StoreEngine()`, `StorePath()`, `StoreDSN()`, `StorePoolMaxConns()`) without depending on `core/config`. `config.Meta` satisfies this interface structurally, preventing circular import dependencies.
4. **Strict Unknown-Key Rejection**:
   TOML files containing unrecognized keys are rejected at load time. If a deprecated setting (such as `web.notes_mode`) is present, `Load` emits an actionable diagnostic explaining why the setting was deprecated and what replaced it.
5. **Robust Password Redaction**:
   `RedactDSN` parses and masks passwords in both standard URL formats (`postgres://user:pass@host/db`) and libpq keyword strings (`host=... password='...'`), honoring escape sequences and whitespace formatting.

---

## Usage Examples

### Loading Configuration

```go
package main

import (
    "errors"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/config"
)

func main() {
    // Load config from standard path hierarchy (or defaults if missing)
    cfg, err := config.Load("")
    if err != nil {
        if errors.Is(err, config.ErrInvalid) {
            log.Fatalf("Invalid configuration: %v", err)
        }
        log.Fatalf("Filesystem error loading configuration: %v", err)
    }

    fmt.Printf("Config loaded from: %s\n", cfg.SourcePath())
    fmt.Printf("Meta store engine: %s\n", cfg.Meta.Engine)
}
```

### Resolving Listen / Dial Endpoint

```go
func setupServer(cfg config.Config) {
    endpoint, err := cfg.Server.Endpoint()
    if err != nil {
        log.Fatalf("Failed to resolve server endpoint: %v", err)
    }

    if endpoint.IsLocal() {
        fmt.Printf("Listening on local unix socket: %s\n", endpoint.Address)
    } else {
        fmt.Printf("Listening on TCP address: %s\n", endpoint.Address)
    }
}
```

### Validating In-Memory Configuration

```go
func validateCustomConfig() {
    cfg := config.Default()
    cfg.Exec.PoolMaxConns = 16
    cfg.FrontDoor.Enabled = true
    cfg.Exec.MaxTargetConns = 32
    cfg.FrontDoor.TLSCertFile = "/etc/autodb/cert.pem"
    cfg.FrontDoor.TLSKeyFile = "/etc/autodb/key.pem"
    cfg.FrontDoor.TLSHostNames = []string{"db.internal"}

    if err := cfg.Validate(); err != nil {
        log.Fatalf("Configuration validation failed: %v", err)
    }
}
```

### Redacting DSNs for Logging

```go
func logConnection(dsn string) {
    sanitized := config.RedactDSN(dsn)
    log.Printf("Connecting to metadata database: %s", sanitized)
}
```
