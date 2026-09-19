# rpc

`rpc` provides the Msgpack-RPC management and query server for `autodb`. Built upon the `golib/server/rpc` transport, it serves as the control plane for the Neovim plugin, the interactive TUI, and administration tooling. It is a strictly mechanical projection of `core/auth` and `core/exec`, enforcing transport authentication, peer IP audit threading, protocol version gating, and deny-before-disclose error masking.

---

## 1. Architectural Overview

```
 ┌────────────────────────┐         ┌────────────────────────┐
 │      Neovim Plugin     │         │      autodb TUI        │
 │ (sockconnect rpc=true) │         │   (Embedded Client)    │
 └───────────┬────────────┘         └───────────┬────────────┘
             │                                  │
             │ Unix Domain Socket / Loopback TCP│
             ▼                                  ▼
 ┌───────────────────────────────────────────────────────────┐
 │                        rpc.Server                         │
 │  ┌─────────────────────────────────────────────────────┐  │
 │  │ Phase 1: sys.hello Handshake & Version Gating       │  │
 │  └──────────────────────────┬──────────────────────────┘  │
 │                             ▼                             │
 │  ┌─────────────────────────────────────────────────────┐  │
 │  │ Phase 2: Inbound Msgpack Decode (Bounds: 2 MiB max) │  │
 │  └──────────────────────────┬──────────────────────────┘  │
 │                             ▼                             │
 │  ┌─────────────────────────────────────────────────────┐  │
 │  │ Phase 3: In-Band Bearer Token & Peer IP Injection   │  │
 │  └──────────────────────────┬──────────────────────────┘  │
 │                             ▼                             │
 │  ┌─────────────────────────────────────────────────────┐  │
 │  │ Phase 4: Method Dispatch (sys, auth, conn, exec, …) │  │
 │  └──────────────────────────┬──────────────────────────┘  │
 │                             ▼                             │
 │  ┌─────────────────────────────────────────────────────┐  │
 │  │ Phase 5: Result Normalization & Error Disclosure    │  │
 │  └─────────────────────────────────────────────────────┘  │
 └─────────────────────────────┬─────────────────────────────┘
                               │
            ┌──────────────────┴──────────────────┐
            ▼                                     ▼
   ┌─────────────────┐                   ┌─────────────────┐
   │    core/auth    │                   │    core/exec    │
   │ (Auth, Grants,  │                   │ (Engines, Pools,│
   │  PATs, Keyslot) │                   │  Lanes, Session)│
   └─────────────────┘                   └─────────────────┘
```

---

## 2. Handshake & Version Gating State Machine

Before executing administrative or query commands, every new transport connection must successfully execute `sys.hello`. The server enforces a single, non-negotiable integer protocol version (`Protocol = 7`):

```
                     [ Client Connects ]
                              │
                              ▼
                     [ Wait for sys.hello ]
                              │
            First request sys.hello(clientInfo)?
            ┌─────────────────┴─────────────────┐
           NO                                  YES
            │                                   │
            ▼                                   ▼
   [Refuse with Code]                Matches Protocol == 7?
   CodeHandshakeRequired             ┌──────────┴──────────┐
   (-32021)                         NO                    YES
                                     │                     │
                                     ▼                     ▼
                            [Refuse with Code]     [Session Admitted]
                            CodeProtocolMismatch   • sessHello = true
                            (-32020)               • Return server version,
                                     │               instance ID & notes
                                     ▼
                            [Session Poisoned]
                            (All future requests
                             immediately denied)
```

### Handshake Invariants
- **Strict Integer Lock**: The server speaks exactly one protocol version. A mismatch immediately poisons the session, signaling Neovim to prompt re-provisioning of the daemon binary.
- **Probes**: A `sys.hello` without a `protocol` field is treated as an unauthenticated probe (used by single-instance checks and CLI detection). It receives server metadata but is not admitted to the method surface.
- **Zero Business Logic**: No SQL execution or authentication occurs in the handshake.

---

## 3. Method Surface Inventory

The RPC server exposes methods grouped into seven distinct domain namespaces:

```
┌──────────────┬──────────────────────────────────┬────────────────────────────────────┐
│ Namespace    │ Primary Verbs                    │ Description                        │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `sys.*`      │ `hello`, `ping`, `shutdown`,     │ Daemon health, handshake, pressure │
│              │ `pressure`, `instance`, `verbs`  │ telemetry, and graceful draining.  │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `auth.*`     │ `bootstrap`, `login`, `logout`,  │ User authentication, PBKDF2/Argon2 │
│              │ `whoami`, `user_create`,         │ passphrases, role administration,  │
│              │ `grant_add`, `pat_create`, …     │ and PAT credential management.     │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `conn.*`     │ `create`, `list`, `delete`,      │ Database target connection cards,  │
│              │ `test`, `rename`, `effective_cap`│ driver resolution, and rename.     │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `exec.*`     │ `run`, `run_script`,             │ SQL statement execution, scripts,  │
│              │ `session_open`, `session_run`,   │ multi-statement transactions, and  │
│              │ `session_close`                  │ in-flight stateful ExecSessions.   │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `history.*`  │ `list`, `clear`                  │ Execution history, statement audit │
│              │                                  │ log, and query duration metrics.   │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `keyslot.*`  │ `list`, `add`, `remove`          │ Service keyslot configuration and  │
│              │                                  │ cryptographic key management.      │
├──────────────┼──────────────────────────────────┼────────────────────────────────────┤
│ `ca.*`       │ `pem`                            │ Root and intermediate CA certificate│
│              │                                  │ PEM export for client trust stores.│
└──────────────┴──────────────────────────────────┴────────────────────────────────────┘
```

---

## 4. Stateful Execution: `ExecSession` Pipeline

For multi-step transactional workflows (e.g. `BEGIN`, statements, `COMMIT`), clients utilize the stateful `exec.session_*` verbs:

```
 ┌────────────────┐
 │ exec.session_  │ ──► Allocate backend connection, pin sessionID,
 │     open       │     and enforce per-user/global session caps.
 └───────┬────────┘
         │
         ▼
 ┌────────────────┐     ┌─────────────────────────────────────────┐
 │ exec.session_  │◄───►│ • Enforces exactly ONE in-flight query  │
 │     run        │     │ • Tracks transaction state (T/I/E)      │
 └───────┬────────┘     │ • Bounds execution time via ctx deadline│
         │              └─────────────────────────────────────────┘
         ▼
 ┌────────────────┐
 │ exec.session_  │ ──► Roll back uncommitted transactions, release
 │     close      │     backend permit, and return to idle pool.
 └────────────────┘
```

### Safety Invariants
- **Concurrency Guard**: An `ExecSession` permits at most one concurrent in-flight query (`CodeSessionBusy` returned on overlap).
- **Transaction State Machine**: Verifies SQL commands against the session's active transaction state (`CodeTxState`).
- **Bounded Retention**: Idle sessions are reclaimed by background janitors if left unclosed by orphaned clients.

---

## 5. Deny-Before-Disclose Error Taxonomy

To prevent information disclosure and side-channel leakage to unauthorized callers, `rpc` sanitizes all errors using an explicit allowlist mapping (`publicErrs`):

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Error Translation Pipeline                      │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
                       Internal Error Returned
                                    │
                   Matches publicErrs allowlist?
                   ┌────────────────┴────────────────┐
                  YES                               NO
                   │                                 │
                   ▼                                 ▼
         [Public Error Code]              [Opaque Generic Error]
         Return registered wire code      Return -32603 Internal Error
         and public sentinel message      "internal error"
```

### Wire Error Codes

| Wire Code | Constant | Meaning / Client Remediation |
| :--- | :--- | :--- |
| `-32020` | `CodeProtocolMismatch` | Client/server version mismatch. Re-provision client binary. |
| `-32021` | `CodeHandshakeRequired` | Attempted to call verbs before `sys.hello`. |
| `-32030` | `CodeAuth` | Authentication failure (invalid credentials, expired token, locked store). |
| `-32031` | `CodeDenied` | Authorization denial (insufficient grants, IP not allowlisted). |
| `-32032` | `CodeStatementRejected` | Query gate violation (WHERE-less mutation, statement limit exceeded). |
| `-32040` | `CodeSessionNotFound` | Unknown or expired `ExecSession` ID. Re-open session. |
| `-32041` | `CodeSessionBusy` | Query already in flight on this `ExecSession`. Await completion. |
| `-32042` | `CodeSessionCapExceeded` | Too many open sessions. Close idle sessions before opening new ones. |
| `-32043` | `CodeTxState` | Invalid query for current transaction state (e.g. `COMMIT` when idle). |
| `-32044` | `CodeConnectionDraining` | Connection or database target is shutting down. Do not retry. |
| `-32045` | `CodeNoSuchTx` | Transaction ID not found or already completed. |
| `-32046` | `CodeInvalidToken` | PAT management validation failure (invalid lifetime, name collision). |
| `-32047` | `CodeKeyslot` | Keyslot operation failure (file permissions, duplicate slot). |

---

## 6. Domain Jargon Glossary

| Term | Definition |
| :--- | :--- |
| **Msgpack-RPC** | Binary RPC protocol using MessagePack serialization over TCP or Unix sockets, adhering to standard RPC request/response framing. |
| **Protocol 7** | The current wire protocol revision. Bumped strictly whenever methods or payload semantics change. |
| **Handshake Gating** | Enforced security policy where every method (except `sys.hello`) is blocked until a successful `sys.hello` exchange occurs. |
| **Bearer Token** | In-band authentication token passed as the first parameter to secured RPC calls, identifying the user and authorized scope. |
| **Peer IP Injection** | Extraction of the remote TCP peer IP at the transport layer, threaded directly into `core/auth` to enforce IP allowlists and audit integrity. |
| **ExecSession** | Server-side stateful transaction handle allowing pipelined queries across an exclusive backend database connection. |
| **Deny-Before-Disclose** | Security design principle where raw internal error strings are withheld by default, disclosing only pre-approved public sentinels. |
| **Probe** | Lightweight `sys.hello` check with no protocol version specified, used to test socket occupancy without establishing an admitted session. |
| **Keyslot** | Cryptographic key slot managed by `core/auth` holding encrypted credentials or root key material on disk. |

---

## 7. Go Integration Example

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/rpc"
)

func main() {
	// 1. Initialize core services
	cfg := config.DefaultConfig()
	authSvc, err := auth.NewService(cfg.Auth)
	if err != nil {
		log.Fatalf("failed to initialize auth: %v", err)
	}

	engine, err := exec.NewEngine(cfg.Exec, authSvc)
	if err != nil {
		log.Fatalf("failed to initialize engine: %v", err)
	}

	// 2. Create RPC server instance
	srv := rpc.NewServer(authSvc, engine, "v1.4.0", "/tmp/notes")

	// 3. Bind TCP or Unix socket listener
	ln, err := net.Listen("tcp", "127.0.0.1:54320")
	if err != nil {
		log.Fatalf("failed to bind RPC listener: %v", err)
	}
	defer ln.Close()

	fmt.Printf("autodb RPC server listening on %s (Protocol version %d)\n", ln.Addr(), rpc.Protocol)

	// 4. Serve requests
	ctx := context.Background()
	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatalf("server terminated: %v", err)
	}
}
```
