# rpc

autodb's msgpack-RPC server: a mechanical projection of `core/auth`, `core/exec`, and `core/meta` onto the golib RPC transport (`golib/server/rpc` + `msgpackrpc`). Both the Neovim plugin (via `vim.fn.sockconnect("tcp", ..., {rpc = true})` or Unix socket) and the standalone TUI frontend consume this exact same interface.

---

## 1. Architectural Role & Principles

The `rpc` package bridges external clients to autodb's core engines while preserving critical security invariants:

```
                      +-----------------------------+
                      |   Neovim Plugin (Lua RPC)   |
                      +--------------+--------------+
                                     |
                      +--------------v--------------+
                      |   Standalone TUI Frontend   |
                      +--------------+--------------+
                                     | (Unix Socket / Loopback TCP)
                                     v
+-------------------------------------------------------------------------+
|                               rpc.Server                                |
|  * Handshake Gate (sys.hello)              * Input Bounds Validation    |
|  * Peer IP Attribution                     * Wire Error Projection      |
+--------------------+-------------------------------+--------------------+
                     |                               |
                     v                               v
         +-----------------------+       +-----------------------+
         |       core/auth       |       |       core/exec       |
         |  * Credentials / PATs |       |  * SQL Execution      |
         |  * Role-Based Access  |       |  * Statement Admission|
         |  * Client Allowlist   |       |  * Pinned Wire/Session|
         +-----------------------+       +-----------------------+
```

### Core Invariants

1. **Mechanical Projection (Zero Business Logic)**: Handlers perform no independent authorization checks, schema introspection, or SQL execution. They decode positional arguments, enrich the request context with caller metadata, invoke the security and execution cores, and normalize results.
2. **Deny-Before-Disclose**: Internal infrastructure errors, database driver internals, and server stack traces never reach the wire. Unclassified errors are mapped to generic opaque codes.
3. **Peer IP Attribution**: The client's physical IP address (or `127.0.0.1` for Unix sockets) is extracted upon connection acceptance and threaded through every request context to enforce client CIDR allowlists and populate audit logs.
4. **Strict Protocol Versioning**: autodb enforces a single wire protocol version (`rpc.Protocol`). Any incompatible client is stopped at the handshake boundary and instructed to re-provision.

---

## 2. Handshake & Lifecycle Protocol

Every fresh connection begins in an **unverified** state. No administrative, query, or metadata methods can be invoked prior to completing the handshake:

```
               [Client Connects]
                       │
                       ▼
            [Initial Request Frame]
                       │
         ┌─────────────┴─────────────┐
         │                           │
  [Method == sys.hello]       [Method != sys.hello]
         │                           │
         ▼                           ▼
  [Inspect Protocol]          [Immediate Rejection]
         │                    CodeHandshakeRequired (-32021)
         ├─────────────────────────────────────────────┐
         │                                             │
Protocol == rpc.Protocol                       Protocol Mismatch / Missing
         │                                             │
         ▼                                             ▼
  [Session Admitted]                    ┌──────────────┴──────────────┐
  • Sets sessHello = true               │ Protocol Missing (Probe)    │ Protocol != rpc.Protocol
  • Grants access to methods            ▼                             ▼
  • Returns server info & instance ID   [Probe Answered]              [Session Poisoned]
                                        • Returns version & server    • Sets sessRefused = true
                                        • Session remains unadmitted  • Audits protocol error
                                        • Closes after answer         • All future calls fail
```

### Probing Existing Daemons

Before binding a socket or port, autodb uses `rpc.Probe` to check for an existing daemon occupant:
- If a running autodb answers the probe: the starting process exits cleanly (exit code 0), allowing client frontends to reuse the daemon.
- If an alien service occupies the port: `rpc.Probe` returns `ErrNotAutodb`, alerting the operator to a port conflict.
- If dial fails: the process proceeds to bind and initialize storage.

---

## 3. RPC Method Catalog

All methods accept positional arguments and return structured msgpack payloads.

### System (`sys.*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `sys.hello` | `[clientInfo map]` | `HelloResult` | Handshake and protocol verification; returns server version, protocol, instance ID, and notes directory. |
| `sys.ping` | `[]` | `"pong"` | Liveness check; available without authentication once admitted. |
| `sys.shutdown` | `[token]` | `nil` | Gracefully shuts down the autodb daemon (requires admin privileges). |

### Authentication & Users (`auth.*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `auth.needs_bootstrap` | `[]` | `bool` | Checks if master administrative credentials must be initialized. |
| `auth.bootstrap` | `[username, passphrase]` | `Session` | Creates the root administrative user during first-run ceremony. |
| `auth.login` | `[username, passphrase]` | `Session` | Authenticates a user and issues a bearer session token. |
| `auth.logout` | `[token]` | `nil` | Revokes an active session token. |
| `auth.whoami` | `[token]` | `UserInfo` | Returns username, role, allowed IP rules, and session expiration. |
| `auth.user_create` | `[token, username, role, pass]` | `nil` | Creates a new user (Admin only). |
| `auth.user_role` | `[token, username, role]` | `nil` | Changes a user's role (Admin only). |
| `auth.user_disable` | `[token, username, disabled]` | `nil` | Enables or disables a user account. |
| `auth.user_remove` | `[token, username]` | `nil` | Deletes a user account. |
| `auth.passphrase_change`| `[token, oldPass, newPass]` | `nil` | Self-service passphrase modification. |
| `auth.passphrase_reset` | `[token, username, newPass]` | `nil` | Administrative passphrase override. |
| `auth.grant_add` | `[token, username, connID]` | `nil` | Grants a user access to a configured target connection. |
| `auth.grant_remove` | `[token, username, connID]` | `nil` | Revokes a connection grant. |
| `auth.allowlist_add` | `[token, username, cidr]` | `nil` | Restricts a user to specific client IP CIDR blocks. |
| `auth.allowlist_remove`| `[token, username, cidr]` | `nil` | Removes an IP CIDR restriction. |

### Personal Access Tokens (`auth.pat_*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `auth.pat_mint` | `[token, name, connID, ttl, ips]` | `PATSecret` | Issues a bearer PAT for PostgreSQL frontdoor access. Plaintext returned once. |
| `auth.pat_list` | `[token]` | `[]PATInfo` | Lists active PAT metadata (never reveals token secrets). |
| `auth.pat_revoke` | `[token, patID]` | `nil` | Revokes a personal access token immediately. |

### Connections (`conn.*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `conn.create` | `[token, name, engine, dsn]` | `ConnID` | Registers and encrypts a database connection target. |
| `conn.list` | `[token]` | `[]ConnInfo` | Lists target connections accessible to the calling user. |
| `conn.delete` | `[token, connID]` | `nil` | Removes a target connection. |
| `conn.test` | `[token, connID]` | `TestResult` | Validates network connectivity and credentials to backend target. |

### Query Execution (`exec.*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `exec.run` | `[token, connID, sql]` | `RunResult` | Evaluates admission and executes a single SQL query via connection pool. |
| `exec.run_script` | `[token, connID, script]` | `ScriptResult` | Evaluates admission and executes a multi-statement SQL script. |
| `exec.session_open` | `[token, connID]` | `SessionHandle`| Pins a dedicated backend connection for stateful transactions. |
| `exec.session_run` | `[token, handle, sql]` | `RunResult` | Executes SQL on a pinned backend session. |
| `exec.session_close`| `[token, handle]` | `nil` | Releases a pinned backend session back to the pool. |

### Schema & Metadata (`schema.*`)
| Method | Arguments | Returns | Description |
| :--- | :--- | :--- | :--- |
| `schema.tables` | `[token, connID]` | `[]TableInfo` | Introspects table names, types, and row count estimates. |
| `schema.columns` | `[token, connID, table]` | `[]ColInfo` | Introspects column names, types, nullability, and primary keys. |

---

## 4. Wire Error Codes & Mapping Rationale

`rpc.wireErr` standardizes all error reporting into negative numeric codes:

| Code | Identifier | Description & Recommended Client Action |
| :--- | :--- | :--- |
| `-32020` | `CodeProtocolMismatch` | Protocol version incompatibility. Client should update or re-provision. |
| `-32021` | `CodeHandshakeRequired` | Connection attempted commands prior to `sys.hello`. |
| `-32030` | `CodeAuth` | Authentication failure: invalid credentials, expired session, locked keyslot. |
| `-32031` | `CodeDenied` | Authorization failure: role lacks permission, no connection grant, IP rejected. |
| `-32032` | `CodeStatementRejected`| Admission violation: destructive query without WHERE, unauthorized DDL. |
| `-32040` | `CodeSessionNotFound` | Pinned execution session handle invalid or expired. |
| `-32041` | `CodeSessionBusy` | A statement is already in flight on this pinned session. |
| `-32042` | `CodeSessionCapExceeded` | Maximum concurrent sessions reached. Close existing sessions. |
| `-32043` | `CodeTxState` | Query invalid for current transaction state (e.g. DML on aborted tx). |
| `-32044` | `CodeConnectionDraining`| Database target is shutting down or being deleted. |
| `-32045` | `CodeNoSuchTx` | Transaction ID does not exist or has already completed. |
| `-32046` | `CodeInvalidToken` | PAT validation failed (duplicate name, invalid IP constraint). |
| `-32047` | `CodeKeyslot` | Service keyslot operational error. |
| `-32048` | `CodeKeyslotUnavailable`| Storage keyslot is unconfigured or locked. |
| `-32049` | `CodeKeyslotActive` | Action rejected because the storage keyslot is already unlocked. |

---

## 5. Usage Example (Neovim Lua)

```lua
local socket_path = "/home/user/.local/share/autodb/autodb.sock"
local channel = vim.fn.sockconnect("pipe", socket_path, { rpc = true })

-- 1. Handshake
local ok, hello = pcall(vim.rpcrequest, channel, "sys.hello", {
  client = "neovim-autodb",
  protocol = 5,
})
if not ok then
  error("Failed to perform autodb handshake: " .. vim.inspect(hello))
end

-- 2. Authenticate
local session = vim.rpcrequest(channel, "auth.login", "admin", "secret_passphrase")

-- 3. Execute Query
local res = vim.rpcrequest(channel, "exec.run", session.token, "conn-pg-prod", "SELECT id, name FROM users LIMIT 10;")
print("Query returned " .. #res.rows .. " rows in " .. res.duration_ms .. "ms")
```
