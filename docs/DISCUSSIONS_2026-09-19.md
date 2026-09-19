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

## 2. Cross-Subsystem Architectural Observations

### 2.1 AST Reflection Coupling in `core/engine`
- In `core/engine/capabilities.go`, exported receiver methods on `Name` are verified by AST inspection in `capabilities_test.go` (`TestEachPredicateReadsItsOwnField`), requiring each method to strictly read `capsByName[n].<field>`.
- *Observation*: While effective at preventing semantic divergence between capability names and backing fields, new helper methods cannot be added without updating test reflection expectations.
- *Recommendation*: Document AST test coupling in `core/engine/README.md` (already done) and provide compiler-enforced interfaces rather than test-time AST assertions where possible.

### 2.2 Dynamic Capacity versus Fixed Window in `core/pressure`
- In `core/pressure`, rate calculations are hardcoded to a 7-bucket sliding window with 1-second ticks.
- *Observation*: In high-throughput deployments with sub-millisecond burst traffic, a 7-second sliding window may react too slowly to acute connection surges.
- *Recommendation*: Consider exposing window bucket count and tick interval as configuration options in `core/config`, with sane defaults.

### 2.3 Orthogonal Triad in `core/outcome`
- Decoupling Kind (structural category) from Charge (throttle attribution) and Wire Projection (authorization-dependent disclosure) provides a solid foundation for client security.
- *Observation*: `frontdoor` and `rpc` should systematically adopt the Authorization Witness pattern (`outcome.Authorized()`) across all error paths to prevent telemetry leakage to unauthenticated probes.

---

## 3. Prioritized Action Items for Next Sprints

| Priority | Item | Component | Complexity | Description |
| :--- | :--- | :--- | :--- | :--- |
| **High** | Replace per-wait goroutines in `generalLane.reserve` | `frontdoor` | Medium | Mitigate goroutine churn and timer allocations under memory contention. |
| **Medium** | Reconcile `gate/*` error IDs in `held_objects.go` | `frontdoor` | Low | Unify namespace under `frontdoor/*` with backward-compatible audit aliases. |
| **Medium** | Standardize linear ownership tokens in `rpc` / `webserver` | `rpc`, `webserver` | Medium | Apply `acceptToken` LIFO pattern to streaming RPCs and HTTP websocket upgrades. |
| **Low** | Configurable pressure rate sliding window | `core/pressure` | Low | Allow tuning of window buckets for high-frequency burst environments. |
