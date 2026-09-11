// Package auth is autodb's security and cryptographic core.
//
// It encapsulates identity management, user authentication, session lifecycles,
// role-based authorization (RBAC), connection grants, personal access tokens (PATs),
// IP/CIDR allowlists, and cryptographic keyslot envelope encryption.
//
// Every frontend surface (RPC, TUI, Neovim, Web UI, Front Door wire proxy) routes
// through this package—no authentication or authorization logic exists outside it.
//
// ============================================================================
// KEYSLOT ENVELOPE ENCRYPTION ARCHITECTURE (LUKS PATTERN)
// ============================================================================
//
// Connection credentials (DSNs, passwords) are encrypted at rest with a single
// random 32-byte master key per installation. The master key is never stored in
// cleartext. Instead, it is stored in multiple independent "keyslots":
//
//	User Passphrase                   Service Keyfile (/etc/autodb/service.key)
//	       │                                            │
//	       ▼                                            ▼
//	   [Argon2id]                                 [HKDF-SHA256]
//	       │                                            │
//	       ▼                                            ▼
//	  User KEK (32B)                              Service KEK (32B)
//	       │                                            │
//	       ▼                                            ▼
//	[AES-256-GCM Unwrap]                       [AES-256-GCM Unwrap]
//	(AAD: "autodb:keyslot:v1")                 (AAD: "autodb:keyslot:service:v1")
//	       │                                            │
//	       └─────────────────────┬──────────────────────┘
//	                             │
//	                             ▼
//	                 Master Key (in-memory 32B)
//	                             │
//	                             ▼
//	                    [AES-256-GCM Decrypt]
//	                             │
//	                             ▼
//	                 Connection DSN / Password
//
// On startup, Service is "locked". It unlocks when either:
//  1. A user logs in with their passphrase (unwrapping their user keyslot).
//  2. The daemon reads a local 0600 service keyfile (unwrapping the service keyslot).
//
// Once unlocked, the master key resides strictly in process memory and is wiped on exit.
//
// ============================================================================
// RBAC ROLES & STANDING AUTHORITY
// ============================================================================
//
// Authorization combines global user roles with per-connection grants:
//
//	                 Global User Role (reader < editor < admin)
//	                                     │
//	                                     ▼
//	                Connection Grant (reader < editor < admin)
//	                                     │
//	                                     ▼
//	                 Resolved Standing (Effective Permission)
//
// Invariants:
//   - An admin globally still requires an explicit connection grant to execute on a target.
//   - A global reader can never exceed SELECT operations, regardless of connection grants.
//   - Standing is re-evaluated fresh per statement (never cached across transactions).
package auth

