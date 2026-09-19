# core/auth

`core/auth` is `autodb`'s security core. It provides the unified authentication, authorization, secret encryption, IP perimeter admission, keyslot envelope management, and audit logging engine.

Every autodb interface—the PostgreSQL frontdoor wire protocol, interactive terminal UI (TUI), remote procedure call (RPC) daemon, and administrative tools—passes through `core/auth`. No security or authorization decisions exist outside this package.

---

## Architecture & Visual Diagrams

### 1. Dual Keyslot & Master Key Envelope Architecture

Connection secrets (passwords, SSL private keys, connection strings) are sealed using AES-256-GCM under an install-wide 32-byte Master Key (Data Encryption Key / DEK). The Master Key is never persisted in plaintext on disk.

To support both human-interactive unlocks and headless daemon reboots, `core/auth` implements a dual-keyslot model inspired by LUKS:
1. **Passphrase Keyslot (Per-User)**: Wrapped by a Key Encryption Key (KEK) derived from the user's passphrase via Argon2id (RFC 9106).
2. **Service Keyslot (Unattended Daemon)**: Wrapped by a KEK derived from a 32-byte CSPRNG keyfile via HKDF-SHA256 (RFC 5869).

```
   [Human Operator]                                [Headless Server Boot]
          │                                                  │
          ▼                                                  ▼
     Passphrase                                    Service Keyfile (0600)
          │                                                  │
          ▼                                                  ▼
   Argon2id KDF                                         HKDF-SHA256
 (RFC 9106 Profile)                                  (RFC 5869 Info Tag)
   64MB, t=1, p=4                                   "autodb:keyslot:service:kek:v1"
          │                                                  │
    ┌─────┴──────────────────┐                               │
    ▼                        ▼                               ▼
User KEK (32B)       Auth Half (32B)                 Service KEK (32B)
    │                        │                               │
    │                   SHA-256 Digest                       │
    │                        │                               │
    │                 Verify vs Stored                       │
    │                 PassHash Record                        │
    ▼                        ▼                               ▼
AES-256-GCM Open      [Login Verified]               AES-256-GCM Open
AAD: "autodb:mk:v1"                                  AAD: "autodb:keyslot:service:v1"
    │                                                        │
    └───────────────────────┬────────────────────────────────┘
                            │
                            ▼
              ┌───────────────────────────┐
              │  Master Key in Memory     │ (Never written to disk)
              │  (32 bytes, wiped on halt)│
              └─────────────┬─────────────┘
                            │
         ┌──────────────────┴──────────────────┐
         ▼                                     ▼
  EncryptSecret(plaintext, connID)      DecryptSecret(blob, connID)
  AES-256-GCM Seal                      AES-256-GCM Open
  AAD: "autodb:conn:<id>:v1"            AAD: "autodb:conn:<id>:v1"
```

#### Cryptographic Envelope Invariants
- **Cryptographic Separation of Slots**: User keyslots use AAD `autodb:mk:v1`, while the service keyslot uses AAD `autodb:keyslot:service:v1`. A database attacker cannot swap ciphertext between slot types.
- **Connection Row Binding (AAD)**: Connection secrets are encrypted with AAD `autodb:conn:<id>:v1`. An attacker with write access to the meta-database cannot move an encrypted secret blob from one connection row to another.
- **Fail-Closed Locking**: Before an initial unlock, all secret operations immediately return `ErrLocked`.

---

### 2. Role Hierarchy & Grant Composition Matrix

Authorization enforces a three-tier role hierarchy. Role rank is strictly ordered:
`reader` (rank 1) < `editor` (rank 2) < `admin` (rank 3).

```
              ┌───────────────────────────┐
              │           admin           │  Rank 3: Full server control, user mgmt,
              │                           │          allowlists, schema & data
              └─────────────┬─────────────┘
                            │
              ┌─────────────▼─────────────┐
              │          editor           │  Rank 2: Schema modifications (DDL),
              │                           │          data modifications (DML)
              └─────────────┬─────────────┘
                            │
              ┌─────────────▼─────────────┐
              │          reader           │  Rank 1: Queries only (SELECT)
              │                           │
              └───────────────────────────┘
```

#### Action Evaluation & Privilege Resolution

Every privileged operation maps to an `Action`:
- `ActionRead`: SELECT queries (minimum rank 1).
- `ActionWrite`: INSERT, UPDATE, DELETE (minimum rank 2).
- `ActionDDL`: ALTER, CREATE, DROP (minimum rank 2).
- `ActionManage`: User creation, grant assignment, IP allowlist management (minimum rank 3).

```
                             [Authorize(token, connID, action)]
                                             │
                                             ▼
                                     [Resolve Token]
                                     • Session or PAT
                                     • Verify User Enabled
                                             │
                                             ▼
                                   Is action == ActionManage?
                                             │
                             YES ────────────┴──────────── NO
                              │                            │
                              ▼                            ▼
                      Does Caller Have             Does Caller Have a Grant
                     Global Admin Role?            on Target Connection ID?
                              │                            │
                     YES ─────┴───── NO           YES ─────┴───── NO
                      │               │            │               │
                      ▼               ▼            ▼               ▼
                  [PERMIT]         [DENY]    Calculate Effective Role:
                                             min(global_role, grant_role)
                                                   │
                                                   ▼
                                             Does Effective Role >=
                                             Required Rank for Action?
                                                   │
                                          YES ─────┴───── NO
                                           │               │
                                           ▼               ▼
                                       [PERMIT]         [DENY]
```

#### Authorization Invariants
- **No Global Bypass**: Admins **must** possess an explicit grant on a connection to perform data or DDL actions against it. Global admin rights do not imply automatic access to customer data.
- **Upper-Bound Role Capping**: A user's effective privilege on a connection is bounded by `min(global_role, grant_role)`. A user with global `reader` standing cannot execute writes even if assigned an `editor` grant.
- **Creator Grant Ceiling**: When creating a connection, the creator receives an ownership grant capped at `editor`. Creation cannot mint connection-admin rights.

---

### 3. Personal Access Token (PAT) Lifecycle & Verification

Personal Access Tokens are bearer credentials designed for frontdoor database access (e.g. psql, application DSNs). They are decoupled from the user's master passphrase: leaking a PAT exposes only a single scoped connection, rather than the user's master encryption keyslot.

```
Credential String:  adb_pat_  [  9-byte Selector  ]  .  [  32-byte Secret  ]
                    └──Prefix─┴──────Base64URL─────┴─Dot┴─────Base64URL────┘
                                        │                           │
                                        ▼                           ▼
                                  Lookup Key                  SHA-256 Digest
                              (Stored Plaintext)            (Stored in Meta DB)
```

#### Verification Pipeline & Constant-Time Decoys

To prevent user-enumeration and credential-probing timing attacks, the PAT verification pipeline executes constant-time comparisons and constant-time decoys:

```
                      [VerifyPAT(presentedToken, clientIP)]
                                        │
                                        ▼
                             [splitPAT(presentedToken)]
                            Extract Selector & Secret
                                        │
                                        ▼
                           Query DB by Selector on Meta
                                        │
                         Record Found? ─┴─ Record Missing?
                              │                  │
                              ▼                  ▼
                    [Compute SHA-256]    [Execute Dummy Decoy Hash]
                            │                    │
                            ▼                    ▼
                   ConstantTimeCompare   ConstantTimeCompare (Dummy)
                            │                    │
                   Match? ──┴── Mismatch?        ▼
                     │             │         [Return ErrPATInvalid]
                     ▼             ▼
               [Check Revoked & [Return ErrPATInvalid]
                 Expiry Times]
                     │
             Valid ──┴── Invalid
               │             │
               ▼             ▼
        [Check User &   [Return ErrPATInvalid]
        Conn Enabled]
               │
        Valid ──┴── Invalid
          │             │
          ▼             ▼
      [Evaluate IP  [Return ErrPATInvalid]
       Admission]
          │
    Pass ──┴── Fail
      │         │
      ▼         ▼
  [Record   [Return ErrPATInvalid]
  LastUsed]
      │
      ▼
   [PERMIT]
```

#### PAT Policy Constraints
- **Connection Scoping**: Every PAT is pinned to exactly one `connID`. Unscoped or wildcard tokens are disallowed.
- **Lifetime Bounds**: Default lifetime is 90 days (`PATDefaultLifetime`); maximum permitted lifetime is 365 days (`PATMaxLifetime`). Non-expiring tokens cannot be created.
- **Token Caps**: A user may hold at most 16 active tokens (`PATMaxPerUser`), with a system-wide cap of 512 active tokens (`PATMaxGlobal`).
- **Coalesced Last-Used Updates**: To avoid saturating meta-store disk I/O on active database workloads, `NotePATUse` coalesces timestamp updates in memory with a 5-minute threshold.

---

### 4. Dual-Layer IP Admission & Token Allowlist Narrowing

Network admission protects the frontdoor wire port and web gateway via a two-tier union model, narrowed optionally by per-token IP filters:

```
                            [Incoming Client IP]
                                      │
                                      ▼
                        Is IP in Global Allowlist?
                 (Config CIDRs ∪ ip_allowlist DB table)
                                      │
                         YES ─────────┴───────── NO
                          │                       │
                          │                       ▼
                          │         Is IP in User's Allowlist?
                          │            (user_ips DB table)
                          │                       │
                          │          YES ─────────┴───────── NO
                          │           │                       │
                          ▼           ▼                       ▼
                     [Admitted by Global]               [Access Denied]
                    [Admitted by User-Row]
                              │
                              ▼
                   Does PAT Define allowed_ips?
                              │
                   YES ───────┴─────── NO
                    │                   │
                    ▼                   ▼
             Is Client IP in        [Admitted]
             PAT allowed_ips?
                    │
            YES ────┴──── NO
             │             │
             ▼             ▼
        [Admitted]  [Access Denied]
```

#### Rationale for the Union Model
- **Global Allowlist**: Houses shared corporate infrastructure (office egress, VPN gateways, production VPC CIDRs).
- **Per-User Allowlist**: Allows individual developers to register dynamic home or remote IPs without bloating global infrastructure rules.
- **Token Allowlist**: Further narrows a specific token's usage (e.g. pinning a CI runner token to a specific subnet).

---

### 5. Standing Authority for Long-Lived Sessions

PostgreSQL wire connections and pinned multi-statement transactions outlive the initial authentication handshake. A client authenticated at 09:00 may attempt a transaction commit at 09:15. If the user was disabled or demoted at 09:10, the in-flight transaction must not proceed unchecked.

`core/auth` provides `ResolveStanding` using typed authority references (`AuthorityRef`):

```
                       [Long-Lived Wire Session / TX]
                                     │
                                     ▼
                     Periodic Recheck: ResolveStanding(ctx, ref)
                                     │
                     ┌───────────────┴───────────────┐
                     ▼                               ▼
          ref.Kind == AuthoritySession     ref.Kind == AuthorityPAT
                     │                               │
                     ▼                               ▼
           Read meta.Sessions Row          Read meta.PersonalAccessTokens Row
           Check Revocation & Expiry       Check Revocation & Expiry
                     │                               │
                     └───────────────┬───────────────┘
                                     │
                                     ▼
                           Read meta.Users Row
                       Verify User Not Disabled
                                     │
                                     ▼
                          Read meta.Grants Row
                    Re-evaluate min(userRole, grantRole)
                                     │
                                     ▼
                   ┌───────────────────────────────────┐
                   │         StandingVerdict           │
                   ├───────────────────────────────────┤
                   │ Standing: bool                    │
                   │ MayWrite: bool                    │
                   │ Role:     string                  │
                   │ Identity: Identity                │
                   │ Reason:   string                  │
                   └─────────────────┬─────────────────┘
                                     │
                  ┌──────────────────┴──────────────────┐
                  │                                     │
                  ▼                                     ▼
         Verdict.Standing == false             Verdict.MayWrite == false
         • Account disabled or                 • Role demoted to reader
           token revoked                       • In-flight write TX rolled back
         • Terminate network connection        • Reader session remains open
```

---

## Domain Jargon

| Term | Definition |
| :--- | :--- |
| **Master Key (DEK)** | The 32-byte cryptographic root key that encrypts all database connection credentials. Kept in process memory; never stored unencrypted. |
| **KEK (Key Encryption Key)** | A key derived from an operator's passphrase or a service keyfile, used solely to wrap/unwrap the Master Key. |
| **Passphrase Keyslot** | User table fields (`kek_salt`, `pass_hash`, `enc_master_key`) storing the master key wrapped under a user's Argon2id-derived key. |
| **Service Keyslot** | System table row (`store_meta`) storing the master key wrapped under a machine-local keyfile for unattended startup. |
| **Unattended Unlock** | The automated process where autodb reads a local 0600 keyfile at startup and unlocks the Master Key without human interaction. |
| **Identity** | An immutable struct (`userID`, `name`, `role`) representing a verified caller, constructed exclusively by `core/auth`. |
| **Grant** | A row in `grants` assigning a user a specific role (`reader` or `editor`) on a target database connection. |
| **PAT (Personal Access Token)** | A bearer token formatted as `adb_pat_<selector>.<secret>`, hashed via SHA-256, and bound to a single connection. |
| **Constant-Time Decoy** | A dummy cryptographic derivation/comparison executed when an invalid username or token selector is supplied, neutralizing timing probes. |
| **Standing Authority** | The continuous, live re-verification of long-lived transactions against database authority records without requiring re-entry of credentials. |
| **Admin Guard Row** | A transactional lock row in `store_meta` (`admin_guard`) that serializes admin demotions and bootstrap operations across concurrent processes. |

---

## Component & File Breakdown

| File | Responsibilities |
| :--- | :--- |
| `doc.go` | Subsystem architecture overview, security model, and threat boundaries. |
| `service.go` | The central `Service` struct, lifecycle options, master key caching, locking primitives, and audit dispatch. |
| `keyslot.go` | Service keyslot lifecycle: keyfile generation, file permission enforcement (0600), enrollment, verification, and unlock. |
| `crypto.go` | Argon2id KDF parameters, HKDF-SHA256 derivation, AES-256-GCM authenticated encryption (`seal`/`open`), and constant-time verification. |
| `authz.go` | Action classification (`read`, `write`, `ddl`, `manage`), role ranking, grant evaluation, and creator ownership assignment. |
| `pat.go` | Personal Access Token minting, validation, lifetime checks, active token caps, and revocation. |
| `pat_allowlist.go` | Token-level IP allowlist verification and canonical CIDR subset matching against user allowlists. |
| `admission.go` | Dual-layer IP admission (`AdmittedByGlobal`, `AdmittedByUserRow`, `AdmittedByTokenList`) and unix-socket exemptions. |
| `sessions.go` | Interactive session creation, token hashing, session token validation, and passphrase logins. |
| `standing.go` | Typed `AuthorityRef` evaluation and `StandingVerdict` generation for long-lived client connections. |
| `users.go` | User management: bootstrap initial admin, add/disable/remove users, change passphrases, and last-admin protections. |
| `allowlist.go` | Administrative management of global IP allowlist rows (`ip_allowlist`). |
| `errors.go` | Package sentinel errors (`ErrLocked`, `ErrDenied`, `ErrTokenInvalid`, `ErrKeyslotCorrupt`, etc.). |

---

## Security Invariants & Attack Mitigations

1. **Defense Against User Enumeration**:
   - When an unrecognised username is presented at login, `dummyDerive` executes a full Argon2id computation against a dummy salt. The response time for an unknown user is indistinguishable from a wrong password for an existing user.
2. **Defense Against PAT Timing Attacks**:
   - If an incoming PAT has an unknown selector, `VerifyPAT` computes a dummy SHA-256 digest and performs a constant-time comparison against a dummy buffer.
3. **Strict File Mode Enforcement**:
   - `readKeyfile` explicitly checks file permissions with `os.Stat`. If permissions allow group or other read access (`perm & 0077 != 0`), it immediately rejects the keyfile with `ErrKeyfileMode`.
4. **Last-Admin Protection**:
   - Disabling, demoting, or removing an administrator runs inside a transactional lock using `lockGuardRow`. It counts active administrators and fails with `ErrLastAdmin` if the operation would leave the system without an enabled administrator.
5. **Atomic Audit Trails**:
   - All privilege mutations (grants, user creation, allowlist changes) write audit records inside the same database transaction (`AuditTxCorrelated`). A mutation cannot succeed if its audit record fails to persist.

---

## Go Usage Examples

### 1. Initializing Service and Unlocking via Service Keyslot

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/auth"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func initAuth(store *meta.Store, keyfilePath string) (*auth.Service, error) {
    svc, err := auth.New(store,
        auth.WithServiceKeyfile(keyfilePath),
        auth.WithConfigAllowlist([]string{"10.0.0.0/8", "192.168.1.0/24"}),
    )
    if err != nil {
        return nil, fmt.Errorf("failed to create auth service: %w", err)
    }

    // Attempt unattended unlock from keyfile
    if err := svc.UnlockWithServiceKeyslot(context.Background()); err != nil {
        log.Printf("Unattended unlock not available: %v (manual login required)", err)
    } else {
        log.Printf("Master key successfully unlocked via service keyslot")
    }

    return svc, nil
}
```

### 2. Authorizing a Connection Action

```go
func executeQuery(ctx context.Context, svc *auth.Service, token string, connID int64, isWrite bool) error {
    action := auth.ActionRead
    if isWrite {
        action = auth.ActionWrite
    }

    ident, err := svc.Authorize(ctx, token, connID, action)
    if err != nil {
        return fmt.Errorf("authorization rejected: %w", err)
    }

    log.Printf("User %s (ID: %d, Role: %s) authorized for %s on connection %d",
        ident.Name(), ident.UserID(), ident.Role(), action, connID)
    return nil
}
```

### 3. Minting and Verifying a PAT

```go
func issueFrontdoorToken(ctx context.Context, svc *auth.Service, sessionToken string, connID int64) (string, error) {
    pat, err := svc.CreatePAT(ctx,
        sessionToken,
        "ci-deployment-worker",
        connID,
        30*24*time.Hour, // 30 days lifetime
        []string{"192.168.1.50/32"}, // Restricted IP
        false,                       // Not cleartext debug
        nil,                         // No new allowlist additions
        "192.168.1.10",              // Creator client IP
    )
    if err != nil {
        return "", fmt.Errorf("failed to mint PAT: %w", err)
    }

    // Return the secret credential once: adb_pat_<selector>.<secret>
    return pat.Secret, nil
}
```
