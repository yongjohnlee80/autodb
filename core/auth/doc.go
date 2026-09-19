// Package auth is autodb's unified security core. It governs identity
// resolution, session lifecycle, fine-grained authorization, connection secret
// encryption, network perimeter admission, unattended keyslots, and an
// append-only transactional audit trail.
//
// Every autodb entry point—the PostgreSQL frontdoor wire protocol, interactive
// terminal UI (TUI), RPC daemon, Lua extensions, and gate-guard HTTP services—
// delegates all security decisions to this package. No authorization logic
// exists outside this package.
//
// # Cryptographic Key Hierarchy & Dual Keyslot Architecture
//
// All sensitive database connection secrets (passwords, SSL keys, connection
// strings) are encrypted with AES-256-GCM under a random 32-byte Master Key
// (Data Encryption Key / DEK). The Master Key is never written to persistent
// storage in plaintext; it is unwrapped into process memory only upon a
// successful keyslot unlock.
//
// To reconcile high security with operational availability, the package provides
// a dual-keyslot model inspired by LUKS:
//
//  1. User Passphrase Keyslot: Encrypted under a Key Encryption Key (KEK)
//     derived from an operator's passphrase using Argon2id (RFC 9106, 64 MiB
//     memory, 1 iteration, 4 threads). The derivation output is split into a
//     32-byte KEK and a 32-byte auth half. The auth half is verified against
//     a stored SHA-256 digest in constant time.
//
//  2. Service Keyslot: Encrypted under a 32-byte KEK derived via HKDF-SHA256
//     (RFC 5869, info "autodb:keyslot:service:kek:v1") from an isolated,
//     0600-permission keyfile on disk. This enables unattended daemon reboots
//     without compromising per-user credentials or exposing raw secrets.
//
// Cryptographic domain separation is strictly enforced via Additional
// Authenticated Data (AAD):
//   - User master key envelopes: AAD "autodb:mk:v1"
//   - Service keyslot envelopes: AAD "autodb:keyslot:service:v1"
//   - Connection secret blobs: AAD "autodb:conn:<id>:v1"
//
// Swapping ciphertext blobs between different users, slots, or connection rows
// results in immediate authentication failure.
//
// # Role Hierarchy & Grant Composition
//
// Authorization models three hierarchical roles:
//
//	reader (rank 1) < editor (rank 2) < admin (rank 3)
//
// Operations are classified into discrete actions:
//   - ActionRead: SELECT queries (minimum rank 1)
//   - ActionWrite: INSERT, UPDATE, DELETE (minimum rank 2)
//   - ActionDDL: ALTER, CREATE, DROP (minimum rank 2)
//   - ActionManage: User creation, grants, allowlists (minimum rank 3)
//
// Account-level administration (ActionManage) requires the global admin role.
// Connection-scoped actions (ActionRead, ActionWrite, ActionDDL) require an
// explicit grant on the target connection, even for administrators. The
// effective privilege on a connection is:
//
//	effectiveRank = min(rankOf(globalRole), rankOf(grantRole))
//
// A global reader can never write to a connection regardless of any assigned
// grant, and a global admin cannot touch a connection without a grant.
//
// # Personal Access Tokens (PAT)
//
// Personal Access Tokens serve as scoped bearer credentials for the PostgreSQL
// frontdoor listener. Each PAT is formatted as:
//
//	adb_pat_<selector>.<secret>
//
// The 9-byte selector is stored in plaintext as an index key, while the 32-byte
// random secret is stored strictly as a SHA-256 digest. The secret is returned
// once upon creation and cannot be recovered from database backups.
//
// To neutralize timing side-channels and user-enumeration attacks, invalid
// tokens trigger constant-time comparisons against deterministic decoy hashes.
// Every PAT is bounded to exactly one connection, constrained to a finite
// lifetime (up to 365 days), and subject to per-user and system-wide active caps.
//
// # Dual-Layer IP Admission
//
// Network admission combines two perimeter layers:
//
//	isAllowed = (globalAllowlist ∪ userAllowlist)
//
// The global allowlist protects shared corporate networks (VPN, office CIDRs),
// while the per-user allowlist permits developers to register remote locations
// without widening the global perimeter. Individual PATs may further narrow
// allowed addresses via token-specific CIDR subsets.
//
// # Standing Authority & Pinned Transactions
//
// Long-lived frontdoor sessions and pinned multi-statement transactions outlive
// individual query calls. The ResolveStanding mechanism continuously re-verifies
// active connections using typed authority references (AuthorityRef) against
// live database records. If an account is disabled or a role demoted during a
// running transaction, ResolveStanding returns a structured StandingVerdict,
// allowing the system to abort write transactions cleanly while keeping
// read-only sessions intact.
//
// # Concurrency & Audit Invariants
//
// Security mutations (user provisioning, role changes, allowlist updates)
// acquire an exclusive transactional guard row (admin_guard in store_meta) to
// prevent check-then-act races across concurrent processes. Every mutation
// writes an audit log row within the same database transaction, ensuring that
// administrative changes cannot commit without an accompanying audit record.
package auth
