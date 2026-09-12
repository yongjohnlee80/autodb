# Architectural Observations: RPC & Frontdoor Protocols

**Date**: 2026-09-12  
**Scope**: `autodb/rpc`, `autodb/frontdoor`  
**Author**: Juliet  

---

## Executive Summary

During the comprehensive documentation and code review of `autodb`'s network boundary packages (`rpc` and `frontdoor`), we evaluated their protocol lifecycle, concurrency guarantees, security boundaries, and error projection patterns. 

Four primary architectural tensions were identified:
1. **RPC Positional Parameter Fragility & Protocol Version Churn**
2. **Extended Query Segment Memory Hold During Pipeline Violations**
3. **Transport-to-Core Coupling in Frontdoor Query Seam**
4. **Connection Poisoning vs. Multiplexed Diagnostic Fallback**

---

## 1. RPC Positional Parameter Fragility & Protocol Version Churn

### Current State
All methods exposed by `rpc.Server` (`methods.go`) decode inbound arguments as positional slices (`args []interface{}`):
```go
// E.g. auth.user_create
func (s *Server) authUserCreate(ctx context.Context, peer string, args []interface{}) (interface{}, error) {
    if len(args) != 4 {
        return nil, golibrpc.NewError(golibrpc.CodeInvalidParams, "auth.user_create requires [token, username, role, passphrase]")
    }
    // ...
}
```
Whenever an additional parameter is introduced (or when a method's transaction semantics change, as seen between Protocol 4 and Protocol 5 for `exec.run_script`), the wire protocol version `rpc.Protocol` must be strictly bumped. An outdated client helloing a newer server is immediately refused with `CodeProtocolMismatch`, forcing Neovim or the TUI to terminate and trigger binary re-provisioning.

### Trade-offs & Tensions
- **Simplicity**: Positional msgpack arrays minimize wire byte overhead and are straightforward for Lua's `vim.rpcrequest` to construct.
- **Fragility**: Any additive field (e.g. optional query timeout, tags, or execution hints) requires either overloading existing parameters or bumping the global protocol constant.

### Recommendation
For future API extensions:
1. Support **named parameter maps** (`map[string]interface{}`) alongside positional arrays for RPC methods.
2. Allow additive, optional fields without incrementing `rpc.Protocol`, reserving protocol version bumps strictly for breaking structural or security changes.

---

## 2. Extended Query Segment Memory Hold During Pipeline Violations

### Current State
In `frontdoor/session_loop.go`, memory consumed by extended query pipelining is budgeted via `segmentLane`:
```go
var seg segmentLane
defer seg.release(l)
```
The segment reservation is released when a `Sync ('S')` message is received from the client, or upon session termination via the deferred cleanup.

### Risk
If a pipelining client issues hundreds of `Parse` and `Bind` messages that trigger an admission violation (`0A000` or `42501`) midway through the pipeline:
1. The server emits an `ErrorResponse` and enters the error state.
2. The remaining pipelined messages before `Sync` are skipped with `ReadyForQuery`.
3. However, if the client delays or fails to transmit the trailing `Sync` message while keeping the socket open, the allocated segment lane bytes remain pinned in memory for the duration of the connection's idle deadline (up to 30 minutes).

### Recommendation
- Explicitly release or clamp the `segmentLane` reservation upon encountering an unrecoverable admission error or syntax violation in `session_extended.go`, rather than deferring exclusively to `Sync` or connection teardown.

---

## 3. Transport-to-Core Coupling in Frontdoor Query Seam

### Current State
`frontdoor.Listener` abstracts its engine dependencies behind three interfaces: `Authenticator`, `CancelExecutor`, and `QueryExecutor`. However, the method signatures return concrete data structures directly from `core/exec`:
```go
type QueryExecutor interface {
    WireQuery(ctx context.Context, req exec.WireQueryRequest) (exec.WireQueryResult, error)
    // ...
}
```
Furthermore, `session_loop.go` directly imports and manipulates `core/admission.Context`, `core/auth`, and `core/exec.SessionID`.

### Risk
The wire protocol handler is coupled to internal execution engine implementation details. Changes to `core/exec` result types or admission context structures require synchronized refactoring across `frontdoor`.

### Recommendation
- Extract a clean, transport-agnostic wire request/result vocabulary (e.g. `core/wire/pgproto`) that decouples wire framing and serialization from the stateful execution engine.

---

## 4. Connection Poisoning vs. Multiplexed Diagnostic Fallback

### Current State
In `rpc/server.go`:
```go
if proto != Protocol {
    sess.Set(sessRefused, true)
    // Audits rpc_protocol_error and rejects all future calls
}
```
Once `sessRefused` is set on a connection, any subsequent request returns `CodeProtocolMismatch` without reading further.

### Observation
While this fail-closed behavior ensures that an incompatible client cannot accidentally issue misparsed mutations, it prevents the client from invoking diagnostic or version introspection methods (e.g. `sys.capabilities` or `sys.ping`) to explain the mismatch to the end user.

### Recommendation
- Retain refusal for all functional and administrative methods, but allow an unauthenticated introspection method (such as `sys.version` or `sys.compatibility_info`) to return actionable diagnostics even after a protocol mismatch.
