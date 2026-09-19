# Architectural Observations & Improvement Proposals (2026-09-19)

This document captures architectural design observations, potential issues, and concrete suggestions for improvement identified during the ongoing repo-wide documentation overhaul and comment refactoring across `autodb`.

---

## 1. Subsystem Analysis: `frontdoor`

### 1.1 Goroutine and Timer Churn in `generalLane.reserve`
- **Location**: `frontdoor/general_lane.go` (`reserve` method, lines 79–92)
- **Mechanism**:
  When a connection attempts to reserve memory against the shared General Lane (e.g. for pending query output or extended segment allocations) and finds `used + n > limit`, it blocks on `l.cond.Wait()`. Because `sync.Cond` does not natively support deadlines, `reserve` spawns a background goroutine for every blocking reservation attempt:
  ```go
  stop := make(chan struct{})
  defer close(stop)
  go func() {
      t := time.NewTimer(budget)
      defer t.Stop()
      select {
      case <-t.C:
          l.cond.Broadcast()
      case <-stop:
      }
  }()
  ```
- **Architectural Risk**:
  Under heavy concurrency where thousands of connections contend for general memory during traffic bursts, spawning a goroutine and allocating a `time.Timer` for each contending reservation creates substantial scheduler overhead and heap allocation pressure. Additionally, `l.cond.Broadcast()` wakes *all* waiting goroutines (thundering herd), rather than only the waiter whose reservation can now be satisfied.
- **Recommendations**:
  1. **Semaphore with Channels**: Replace `sync.Cond` with a channel-based weighted semaphore or a dedicated lane queue where waiting connections register their request and unblock via channel select with a standard timer/context.
  2. **Dedicated Timer Wheel**: If maintaining `sync.Cond`, avoid per-reservation goroutine spawning by maintaining an ordered deadline heap driven by a single background reaper goroutine that signals waiters.

---

### 1.2 Two-Speed Error Namespacing in `held_objects.go`
- **Location**: `frontdoor/held_objects.go` (lines 60–93)
- **Mechanism**:
  Most held-object failure identities are namespaced under `frontdoor/*`:
  - `frontdoor/retained-budget`
  - `frontdoor/duplicate-prepared-statement`
  - `frontdoor/duplicate-portal`
  - `frontdoor/named-object-cap`
  - `frontdoor/param-cap`
  However, two conditions retain legacy prefixes dating back to early classifier implementations:
  - `gate/unknown-statement`
  - `gate/unknown-portal`
- **Architectural Risk**:
  While preserved deliberately for backward compatibility with saved searches in audit log indexers, having heterogeneous namespaces within the same producer (`ProducerHeldObjects`) creates cognitive friction for client developers inspecting PostgreSQL `DETAIL` wire fields.
- **Recommendations**:
  1. **Aliased Transition Period**: Introduce `frontdoor/unknown-statement` and `frontdoor/unknown-portal` as canonical identities, while supporting `gate/*` as alias mappings during audit ingest for a defined transition period.
  2. **Metadata Tagging**: Explicitly document legacy identity mappings in the OpenAPI/schema definitions or registry metadata.

---

### 1.3 Demand-Driven Wake Knock and Read Deadline Semantics
- **Location**: `frontdoor/demand_wake.go` (`knockFor` function, lines 43–54)
- **Mechanism**:
  When the backend scheduler reclaims a connection lease from an idle session, it executes `knockFor`, which sets the connection's read deadline in the past:
  ```go
  conn.SetReadDeadline(now().Add(-time.Second))
  ```
  This unblocks `pgproto3.Backend.Receive()`, allowing the session loop to write a clean PostgreSQL `ErrorResponse` explaining why the connection was terminated. If `SetReadDeadline` fails, the knock tears down the transport directly (`conn.Close()`).
- **Architectural Assessment**:
  - *Strength*: This design cleanly enforces the single-writer invariant (only the session loop goroutine writes to `conn`), preventing concurrent frame interleaving.
  - *Edge Case*: If a non-standard `net.Conn` implementation ignores deadlines or buffers writes asynchronously without honoring deadlines, the session loop could block during error flush.
- **Recommendations**:
  1. Ensure all `conn.Write` calls on the teardown path strictly use bounded write deadlines (`outputStall`).
  2. Maintain metrics/telemetry tracking when `conn.SetReadDeadline` errors out and forces the abrupt transport closure fallback.

---

### 1.4 Linear Ownership Pattern (`acceptToken`) Standardization
- **Location**: `frontdoor/accept_token.go`
- **Mechanism**:
  `acceptToken` encapsulates multiple teardown obligations (WaitGroup counter, ticket reservation, active connection tracking, socket closure, and audit event emission) into a single linear type with atomic CAS protection against double-discharge. It enforces strict LIFO release order:
  1. `untrack()`
  2. `conn.Close()`
  3. `announce()`
  4. `tkt.release()`
  5. `handlerDone()`
- **Architectural Assessment**:
  This is one of the most robust concurrency and resource cleanup patterns in the codebase. It eliminates shutdown race conditions where the listener could exit while tickets remain occupied or sockets remain unclosed.
- **Recommendations**:
  Adopt this explicit linear token pattern across other lifecycle-sensitive boundaries in `rpc` and `webserver` (e.g. client streaming RPC handles and HTTP upgrade sessions).

---

## 2. Subsystem Analysis: `rpc`

### 2.1 Strict Integer Protocol Versioning (`Protocol = 7`)
- **Location**: `rpc/server.go` (lines 23–54)
- **Mechanism**:
  The daemon enforces a single strict integer protocol version (`Protocol = 7`). Any client connecting with a different protocol version is immediately poisoned at `sys.hello` with `CodeProtocolMismatch` (-32020), refusing all subsequent requests:
  ```go
  if info.Protocol != Protocol {
      conn.Set(sessRefused, true)
      return nil, &golibrpc.RPCError{Code: CodeProtocolMismatch, ...}
  }
  ```
- **Architectural Assessment**:
  - *Strength*: Prevents subtle protocol drift and "method not found" runtime surprises when frontends attempt to call newly added verbs (e.g. `conn.rename` or `sys.pressure`).
  - *Trade-off*: Because autodb daemons are long-running background processes, updating the Neovim plugin or TUI binary immediately breaks communication with the active daemon, requiring a full daemon restart and connection dropped.
- **Recommendations**:
  1. **Minimum Supported Protocol Negotiation**: Consider supporting a `MinProtocol` floor (e.g. Protocol 5..7) allowing older frontends to continue operating on existing method subsets without being forcefully poisoned.
  2. **Feature Capability Flags**: Along with version numbers, return a `capabilities: []string` array in the `sys.hello` response so clients can dynamically enable/disable UI actions without bumping the wire integer.

---

### 2.2 Positional Argument Decoding vs Keyed Struct Payloads
- **Location**: `rpc/methods.go` (handlers across `methods.go`)
- **Mechanism**:
  RPC handlers decode incoming arguments as an untyped positional slice `[]any`:
  ```go
  token, _ := args[0].(string)
  connID, _ := args[1].(string)
  sql, _ := args[2].(string)
  ```
- **Architectural Assessment**:
  Positional decoding makes extending existing methods fragile: optional parameters must always be appended to the end of argument lists, and omitting arguments requires transmitting `nil` placeholders.
- **Recommendations**:
  1. For complex administrative operations, migrate toward single-struct dictionary arguments (`map[string]any`) with schema validation.
  2. Maintain positional signatures for high-frequency performance-critical paths (`exec.run`), but document rigid argument schemas.

---

### 2.3 Linear Error Allowlist Matching in `wireErr`
- **Location**: `rpc/methods.go` (`wireErr` function, lines 98–150)
- **Mechanism**:
  `wireErr` performs a sequential linear scan over the `publicErrs` slice using `errors.Is(err, p.sentinel)` for every error produced by the daemon.
- **Architectural Assessment**:
  While secure by default (unmatched errors fall through to opaque generic internal errors), the slice contains over 30 sentinels. In high-frequency query pipelines where errors occur, linear traversal can be optimized.
- **Recommendations**:
  1. Index direct equality matches using a fast lookup table, falling back to `errors.Is` unwrapping only when wrapped error trees are encountered.

---

## 3. Subsystem Analysis: `tui`

### 3.1 State Density in the Root `Model`
- **Location**: `tui/ui.go` (`Model` struct, lines 25–105)
- **Mechanism**:
  The root `Model` struct currently aggregates over 40 distinct fields, managing widget hierarchy, split ratios, active workspace/connection, query execution sequence, authentication attempts, splash banners, note dirty tracking, and editor preference coordination.
- **Architectural Assessment**:
  While consolidating state into a single model simplifies Neovim-style bubble event handling at the tail of the keymap chain, it creates high coupling. Methods in `ui.go`, `commands.go`, `managers.go`, and `explorer.go` directly mutate disparate model fields, making state transitions harder to audit.
- **Recommendations**:
  1. **Modular Sub-Controllers**: Partition `Model` into logical sub-states:
     - `LayoutState`: split ratios, active pane, zoom state, `lastPane`.
     - `ExecutionState`: `execSeq`, `running`, query cancel handles, pagination.
     - `AuthState`: `identityEpoch`, `authSeq`, tokens, user profile.
     - `NotesState`: `curNote`, `noteGen`, `noteDirty`.
  2. Maintain `Model` as the orchestrator embedding these sub-controllers.

---

### 3.2 Generation Fencing vs Server-Side Context Cancellation
- **Location**: `tui/ui.go` (lines 22–24, `execSeq` in query execution)
- **Mechanism**:
  When a user re-executes a query or navigates away, `execSeq` increments. When previous asynchronous RPC tasks complete, their callbacks compare the captured sequence against `m.execSeq` and discard outdated results.
- **Architectural Assessment**:
  - *Strength*: Perfectly guards the UI against race conditions and out-of-order UI rendering without requiring mutexes across the UI loop.
  - *Opportunity*: While discarded on the client side, long-running queries continue executing on the remote PostgreSQL backend until completion, consuming database compute and backend connection permits.
- **Recommendations**:
  1. Pair `execSeq` with active context cancellation: store the active `context.CancelFunc` for the running query task, and invoke it when a new execution preempts the old one, transmitting an explicit cancel frame to the daemon.

---

### 3.3 In-Process RPC Loopback Overhead
- **Location**: `tui/client.go`
- **Mechanism**:
  When `autodb --ui` runs in standalone mode (embedding the daemon and TUI in a single process), the TUI still connects through an in-memory loopback transport speaking Msgpack-RPC.
- **Architectural Assessment**:
  - *Strength*: Strictly enforces the single-source-of-truth invariant: all query execution, authorization, and audit logs pass through the exact same code paths whether invoked from Neovim, the CLI, or the TUI.
  - *Trade-off*: Large result sets (e.g. 50,000 rows in the results table) are serialized to Msgpack binary and immediately deserialized within the same process heap.
- **Recommendations**:
  1. For in-process execution, explore an optional zero-copy in-memory channel transport that bypasses binary serialization while preserving the identical `rpc.Client` interface and security intercepts.

---

---

## 4. Subsystem Analysis: `webserver`

### 4.1 Concurrent Login Session Pooling Race Condition
- **Location**: `webserver/sessions.go` (`join` method, lines 72–105)
- **Mechanism**:
  When a user logs in, the HTTP handler dials the daemon and authenticates a fresh `tuiapp.Session`. It then calls `sessions.join(subject, fresh)`. If a pooled session already exists (e.g. from an existing open tab), `fresh` is marked surplus and the caller closes it:
  ```go
  if entry, ok := s.entries[subject]; ok {
      entry.refs++
      return entry.sess, true // true = fresh session is surplus, caller must close
  }
  ```
- **Architectural Assessment**:
  When a user opens multiple tabs simultaneously or refreshes several tabs concurrently, multiple redundant TCP dials and password authentication calls hit the daemon before the first session enters the pool.
- **Recommendations**:
  1. **Single-Flight Coalescing**: Introduce a `singleflight.Group` keyed by username for session acquisition, ensuring concurrent login requests await the first in-flight daemon dial rather than spawning redundant authentication handshakes.

---

### 4.2 Reverse Proxy Ingress and IP Allowlisting
- **Location**: `webserver/gateway.go` (`ListenAddr`, lines 96–97)
- **Mechanism**:
  The gateway enforces a strict loopback posture (`127.0.0.1:port`) by default. IP allowlist checks (`ipallow.Checker`) read `r.RemoteAddr`.
- **Architectural Assessment**:
  - *Strength*: Prevents accidental exposure of the administrative web UI on routable network interfaces.
  - *Constraint*: When deployed in Kubernetes or behind an organizational reverse proxy (e.g. Traefik/Nginx), `r.RemoteAddr` resolves to the proxy's internal IP. The IP allowlist either blocks all clients or permits all clients routed through that proxy.
- **Recommendations**:
  1. Provide an optional `TrustedProxies []netip.Prefix` configuration that securely inspects `X-Forwarded-For` only when the remote address matches a trusted proxy CIDR.

---

### 4.3 Browser Tab Refresh and Session Reconnection Churn
- **Location**: `webserver/sessions.go` and `webserver/gateway.go`
- **Mechanism**:
  A browser reload drops the WebSocket connection, causing `web.Manager` to treat the previous session as detached (starting the 5-minute `DefaultIdle` timer). The newly loaded page establishes a brand new session, increasing `entry.refs`.
- **Architectural Assessment**:
  Rapidly reloading tabs causes reference counts to climb, delaying automatic logout when the user eventually closes their browser.
- **Recommendations**:
  1. Implement persistent client session identifiers in `sessionStorage` allowing reloaded tabs to reclaim their existing detached session slot before creating a new one.

---

## 5. Cross-Subsystem Architectural Observations

### 5.1 AST Reflection Coupling in `core/engine`
- In `core/engine/capabilities.go`, exported receiver methods on `Name` are verified by AST inspection in `capabilities_test.go` (`TestEachPredicateReadsItsOwnField`), requiring each method to strictly read `capsByName[n].<field>`.
- *Observation*: While effective at preventing semantic divergence between capability names and backing fields, new helper methods cannot be added without updating test reflection expectations.
- *Recommendation*: Document AST test coupling in `core/engine/README.md` (already done) and provide compiler-enforced interfaces rather than test-time AST assertions where possible.

### 5.2 Dynamic Capacity versus Fixed Window in `core/pressure`
- In `core/pressure`, rate calculations are hardcoded to a 7-bucket sliding window with 1-second ticks.
- *Observation*: In high-throughput deployments with sub-millisecond burst traffic, a 7-second sliding window may react too slowly to acute connection surges.
- *Recommendation*: Consider exposing window bucket count and tick interval as configuration options in `core/config`, with sane defaults.

### 5.3 Orthogonal Triad in `core/outcome`
- Decoupling Kind (structural category) from Charge (throttle attribution) and Wire Projection (authorization-dependent disclosure) provides a solid foundation for client security.
- *Observation*: `frontdoor` and `rpc` should systematically adopt the Authorization Witness pattern (`outcome.Authorized()`) across all error paths to prevent telemetry leakage to unauthenticated probes.

---

## 6. Prioritized Action Items for Next Sprints

| Priority | Item | Component | Complexity | Description |
| :--- | :--- | :--- | :--- | :--- |
| **High** | Replace per-wait goroutines in `generalLane.reserve` | `frontdoor` | Medium | Mitigate goroutine churn and timer allocations under memory contention. |
| **Medium** | Single-flight login coalescing in `webserver` | `webserver` | Low | Prevent duplicate daemon dials during concurrent tab logins. |
| **Medium** | Pair `execSeq` with active RPC query cancellation | `tui` | Medium | Cancel backend query tasks on preemption to release database compute. |
| **Medium** | Reconcile `gate/*` error IDs in `held_objects.go` | `frontdoor` | Low | Unify namespace under `frontdoor/*` with backward-compatible audit aliases. |
| **Medium** | Minimum protocol floor & capability negotiation | `rpc` | Medium | Allow minor frontend version divergence without immediate connection poisoning. |
| **Medium** | Standardize linear ownership tokens in `rpc` / `webserver` | `rpc`, `webserver` | Medium | Apply `acceptToken` LIFO pattern to streaming RPCs and HTTP websocket upgrades. |
| **Low** | Trusted proxy support for `webserver` IP allowlisting | `webserver` | Medium | Securely parse `X-Forwarded-For` when behind trusted ingress proxies. |
| **Low** | Modularize root `Model` into focused sub-controllers | `tui` | Low | Decompose `ui.go` state density into layout, auth, and execution states. |
| **Low** | Configurable pressure rate sliding window | `core/pressure` | Low | Allow tuning of window buckets for high-frequency burst environments. |



