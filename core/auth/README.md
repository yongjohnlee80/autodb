# Package `auth` — Authentication, Authorization & Cryptography

`auth` is autodb's **security and cryptographic engine**. It manages user identity, session lifecycles, Personal Access Tokens (PATs), role-based access control (RBAC), per-connection grants, CIDR allowlists, and keyslot envelope encryption.

---

## 1. Keyslot Envelope Encryption Architecture (LUKS Pattern)

autodb protects database connection credentials (DSNs, passwords) using envelope encryption modeled after the Linux Unified Key Setup (LUKS) pattern.

```
User Passphrase                   Service Keyfile (/etc/autodb/service.key)
       │                                            │
       ▼                                            ▼
   [Argon2id]                                 [HKDF-SHA256]
       │                                            │
       ▼                                            ▼
  User KEK (32B)                              Service KEK (32B)
       │                                            │
       ▼                                            ▼
[AES-256-GCM Unwrap]                       [AES-256-GCM Unwrap]
(AAD: "autodb:keyslot:v1")                 (AAD: "autodb:keyslot:service:v1")
       │                                            │
       └─────────────────────┬──────────────────────┘
                             │
                             ▼
                 Master Key (in-memory 32B)
                             │
                             ▼
                    [AES-256-GCM Decrypt]
                             │
                             ▼
                 Connection DSN / Password
```

### 1.1 The Master Key
- Each autodb installation generates a random 32-byte cryptographically secure master key.
- The master key is **never written to disk or database in cleartext**.
- The master key is stored in **keyslots**—wrapped copies encrypted with Key Encryption Keys (KEKs).

### 1.2 User Keyslots (Interactive Unlock)
- When a user is created, a KEK is derived from their passphrase using Argon2id.
- The user's row stores the master key wrapped with their KEK using AES-256-GCM with Additional Authenticated Data (`autodb:keyslot:v1`).
- When a user logs in, their passphrase derives the KEK, unwraps the master key into memory, and transitions the service from "locked" to "unlocked".

### 1.3 Service Keyfile (Headless Daemon Reboot)
- In production, server reboots cannot stall waiting for a human administrator to log in.
- The service keyfile slot wraps the master key using a high-entropy 32-byte secret stored in `/etc/autodb/service.key` (mode `0600`).
- KEK derivation uses HKDF-SHA256 with domain-separated info `autodb:keyslot:service:kek:v1`.
- AAD binds the wrap explicitly to `autodb:keyslot:service:v1`, preventing slot substitution attacks.

---

## 2. Role-Based Access Control (RBAC) & Grants

```
                  Global User Role (reader < editor < admin)
                                      │
                                      ▼
                 Connection Grant (reader < editor < admin)
                                      │
                                      ▼
                  Resolved Standing (Effective Permission)
```

### 2.1 Role Hierarchy
- **`reader`**: SELECT queries only. Run within server-enforced read-only transactions.
- **`editor`**: DML mutations (`INSERT`, `UPDATE`, `DELETE`) subject to predicate guards.
- **`admin`**: DDL (`CREATE`, `ALTER`, `DROP`), user administration, and system management.

### 2.2 Per-Connection Granularity
- **Admins are not globally exempt**: An admin without an explicit connection grant on a production target cannot mutate that target.
- **Floor Invariant**: A globally-`reader` user can never execute writes, regardless of what grants exist.

---

## 3. Standing Authority

A long-lived transaction or wire session outlives the request that opened it. Standing authority provides a typed handle to re-verify permissions dynamically:

- **`AuthoritySession`**: References an interactive session row (`auth-session`). Revocation or timeout immediately ends the session.
- **`AuthorityPAT`**: References a Personal Access Token (`pat`) used by front-door wire clients.

---

## 4. Glossary of Terms

- **KEK (Key Encryption Key)**: An ephemeral 32-byte key derived from a passphrase or keyfile used solely to wrap or unwrap the master key.
- **Master Key**: The primary 32-byte symmetric AES-256 key that encrypts stored connection DSNs.
- **Keyslot**: A database row containing the master key wrapped by a specific KEK.
- **AAD (Additional Authenticated Data)**: Cryptographic binding preventing an encrypted payload from being swapped into a different context.
- **Standing**: The resolved authorization status of an active session, re-checked fresh without relying on cached permissions.
- **PAT (Personal Access Token)**: A scoped, revocable API credential for automated tools and wire proxy connections.

---

## 5. Usage Example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/yongjohnlee80/autodb/core/auth"
    "github.com/yongjohnlee80/autodb/core/meta"
)

func main() {
    ctx := context.Background()

    // 1. Initialize auth service with meta-store backing
    var store *meta.Store // Assume initialized
    authSvc := auth.NewService(store, auth.Options{})

    // 2. Unlock service via service keyfile or user login
    err := authSvc.UnlockWithKeyfile("/etc/autodb/service.key")
    if err != nil {
        log.Fatalf("failed to unlock keyslots: %v", err)
    }

    // 3. Authenticate interactive login
    session, err := authSvc.Login(ctx, "johno", "user-passphrase", "127.0.0.1")
    if err != nil {
        log.Fatalf("login failed: %v", err)
    }

    fmt.Printf("Authenticated user %s, session ID: %d\n", session.Username, session.ID)
}
```
