// Package config implements autodb's authoritative configuration subsystem: loading,
// decoding, derivation, validation, and endpoint resolution.
//
// The configuration file is optional: an absent file yields safe, zero-config
// defaults so that a first-run local installation requires no manual ceremony.
// When a configuration file is present, it is decoded with strict unknown-key
// rejection and validated immediately — configuration errors fail fast at Load
// time rather than surfacing at first query or under production load.
//
// ============================================================================
// CONFIGURATION DISCOVERY & RESOLUTION HIERARCHY
// ============================================================================
//
// When no explicit path is passed (--config=""), autodb searches candidate
// locations in a strict, deterministic sequence:
//
//	                 [Start: Resolve Default Path]
//	                               │
//	                               ▼
//	           ┌────────────────────────────────────────┐
//	           │ 1. System Server: /etc/autodb/config.toml│
//	           │    Mode 0640 (Root / Service Account)  │
//	           └───────────────────┬────────────────────┘
//	                               │ Present & Readable?
//	                     YES ──────┴────── NO
//	                      │                │
//	                      ▼                ▼
//	                 [Return Path]   ┌────────────────────────────────────────┐
//	                                 │ 2. User Config:                        │
//	                                 │    $XDG_CONFIG_HOME/autodb/config.toml │
//	                                 │    (~/.config/autodb/config.toml)      │
//	                                 └─────────────────┬──────────────────────┘
//	                                                   │ Present & Readable?
//	                                         YES ──────┴────── NO
//	                                          │                │
//	                                          ▼                ▼
//	                                     [Return Path]   ┌────────────────────────────────────────┐
//	                                                     │ 3. System Client:                      │
//	                                                     │    /etc/autodb/client.toml             │
//	                                                     │    Mode 0644 (World-Readable Client)   │
//	                                                     └─────────────────┬──────────────────────┘
//	                                                                       │ Present & Readable?
//	                                                             YES ──────┴────── NO
//	                                                              │                │
//	                                                              ▼                ▼
//	                                                         [Return Path]   [Return User Path (Defaults)]
//
// ============================================================================
// SERVICE HOST DETECTION & SPOOFING PREVENTION
// ============================================================================
//
// On a multi-user machine running autodb as a systemd service, an unprivileged
// developer running the TUI without configuration must never accidentally spawn
// a private daemon on the service's port against an empty per-user database.
//
// autodb distinguishes file readability (permission to inspect secrets) from
// file existence (presence of a service on this host):
//
//   - ServiceHostSeen: Evaluated via os.Stat on /etc/autodb/config.toml.
//     Returns true even if the file is mode 0640 and unreadable by the caller.
//   - ForeignOnAServiceHost: Evaluated when a loaded config is NOT the system
//     server config on a host where ServiceHostSeen is true. In this state,
//     spawning an embedded background daemon is strictly forbidden.
//
// ============================================================================
// CONNECTION SIZING & FRONTDOOR DERIVATION PIPELINE
// ============================================================================
//
// To prevent wire sessions from monopolizing database connections and starving
// the TUI, administrative queries, or target databases, sizing parameters are
// derived and bound as follows:
//
//	  [Host Hardware]
//	         │
//	         ▼
//	  [PoolMaxConns = 2 × NumCPU()] ─────────────┐
//	  (Default target pool bound)                │
//	                                             ▼
//	                             [DefaultReservedHeadroom(pool)]
//	                             • min(4, pool / 2)
//	                             • Ensures interactive/control queries survive
//	                                             │
//	                                             ▼
//	                             [FrontDoor.EffectiveMaxLeases]
//	                             • Derived: PoolMaxConns - ReservedHeadroom
//	                             • Explicit max_leases may only be lower
//
// Key invariant: A reservation may shrink to fit a small pool, but the pool is
// NEVER automatically inflated to satisfy a reservation — doing so would over-claim
// connection limits on target databases that autodb does not own.
//
// ============================================================================
// TRANSPORT SECURITY CONTRACTS
// ============================================================================
//
//   - Meta-Store DSN: Remote PostgreSQL meta-stores require sslmode=verify-full
//     and an explicit sslrootcert. sslmode=require is explicitly refused because it
//     does not verify server identity, exposing access tokens and connection
//     credentials to man-in-the-middle attacks. An insecure DSN is permitted only
//     via the explicit configuration flag allow_insecure_dsn = true.
//   - PostgreSQL Frontdoor: TLS is mandatory. Disabling TLS requires setting
//     insecure_disable_tls to the exact acknowledgement string:
//     "i-accept-that-every-pat-crosses-in-cleartext".
//
// ============================================================================
// PACKAGE DECOUPLING (core/meta.StoreConfig)
// ============================================================================
//
// To avoid circular dependencies between core/config and core/meta, config.Meta
// structurally implements core/meta.StoreConfig via StoreEngine(), StorePath(),
// StoreDSN(), and StorePoolMaxConns(). Neither package imports the other.
package config
