# Admission chain comparison

This is the Step 9 / A7 before-and-after rendering. The **before** block is a
fixed transcription of production at baseline `aeac553`; it is historical
evidence and is not regenerated. The `O{...}` stage lists in the **current**
block are rendered from the same stage-slice factories used by production in
`core/exec/admission_drive.go`.
`TestAdmissionChainComparison_CurrentBlockMatchesProduction` compares that
block byte-for-byte, so stage movement or deletion requires a visible doc diff.
The surrounding `D` and `R` annotations are review-maintained context, not a
claim that this renderer derives the complete control-flow graph from source.

Notation:

- `O<n>{...}` is one distinct `admission.Orchestrator.Run` invocation. A later
  `O` is not continuation inside the earlier invocation.
- `D{...}` is a direct drive boundary: classification, authorization I/O,
  routing, recording, enforcement, resource accounting, or dispatch.
- `R{...}` is a segment-scoped resource, not a statement stage.
- Profiles are rendered separately because each `profile` stage is constructed
  with that named capability profile even where the route topology is equal.

## Before: baseline aeac553

<!-- admission-chain-baseline-aeac553:begin -->
```text
profile=v1compat
  pooled/ordinary: D{sizecap -> Classify -> profile(v1compat) -> actual-class grant -> fresh policy -> readeranalysis -> guardwhere -> attempt -> per-statement read-only wrap -> dispatch}
  pooled/control: D{sizecap -> Classify -> profile(v1compat) -> off-session control refusal; no later boundary}
  rpc-session/ordinary: D{sizecap -> Classify; control branches away -> readeranalysis -> authorizeunit -> profile(v1compat) -> guardwhere -> tx-state -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-stateful: D{sizecap -> Classify -> control route -> profile(v1compat) -> control floor -> authorizeunit -> tx-state -> pooled session-state allowlist -> RPC SET admin floor -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-transaction: D{sizecap -> Classify -> control route -> profile(v1compat) -> control floor -> ParseTxControl -> handleTxControl}
  wire-simple/decoded-ordinary: D{sizecap -> Classify; control branches away -> readeranalysis -> authorizeunit -> profile(v1compat) -> guardwhere -> tx-state -> attempt -> per-statement read-only wrap -> decoded dispatch}
  wire-simple/decoded-control: D{sizecap -> Classify -> control route -> profile(v1compat) -> control floor -> stateful: tx-state/wire session-state denylist/attempt/read-only wrap/decoded dispatch; transaction: ParseTxControl/handleTxControl}
  wire-simple/raw-ordinary: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> authorizeunit -> profile(v1compat) -> guardwhere -> join -> attempts -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-stateful: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> profile(v1compat) -> control floor -> tx-state -> wire session-state denylist -> join -> attempt -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-transaction: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> profile(v1compat) -> control floor -> join -> attempt -> wireControl -> profile(v1compat) -> control floor -> ParseTxControl -> handleTxControl}
  wire-extended/Parse-ordinary: R{segment read-only wrap acquired at entry} -> D{sizecap -> Classify; empty/control branches away -> readeranalysis -> authorizeunit -> profile(v1compat) -> guardwhere -> statement resource reservation -> target Parse}
  wire-extended/Parse-control: R{segment read-only wrap acquired at entry} -> D{sizecap -> Classify -> deferred control: reserve/store only; admission waits for Execute}
  wire-extended/Execute-ordinary: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy -> authorizeunit -> tx-state + require segment wrap -> attempt -> ExecuteOp}
  wire-extended/Execute-deferred-control: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy -> release segment wrap -> wireControl -> profile(v1compat) -> control floor -> stateful: tx-state/session-state/attempt/dispatch; transaction: ParseTxControl/handleTxControl}
  wire-startup/GUC: D{authenticate -> authorize target -> profile exposure -> reserve -> pin/checkout grammar -> UTF-8 lease -> fresh policy -> admitWireSet -> target SET; repeat per GUC}
profile=session
  pooled/ordinary: D{sizecap -> Classify -> profile(session) -> actual-class grant -> fresh policy -> readeranalysis -> guardwhere -> attempt -> per-statement read-only wrap -> dispatch}
  pooled/control: D{sizecap -> Classify -> profile(session) -> off-session control refusal; no later boundary}
  rpc-session/ordinary: D{sizecap -> Classify; control branches away -> readeranalysis -> authorizeunit -> profile(session) -> guardwhere -> tx-state -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-stateful: D{sizecap -> Classify -> control route -> profile(session) -> control floor -> authorizeunit -> tx-state -> pooled session-state allowlist -> RPC SET admin floor -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-transaction: D{sizecap -> Classify -> control route -> profile(session) -> control floor -> ParseTxControl -> handleTxControl}
  wire-simple/decoded-ordinary: D{sizecap -> Classify; control branches away -> readeranalysis -> authorizeunit -> profile(session) -> guardwhere -> tx-state -> attempt -> per-statement read-only wrap -> decoded dispatch}
  wire-simple/decoded-control: D{sizecap -> Classify -> control route -> profile(session) -> control floor -> stateful: tx-state/wire session-state denylist/attempt/read-only wrap/decoded dispatch; transaction: ParseTxControl/handleTxControl}
  wire-simple/raw-ordinary: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> authorizeunit -> profile(session) -> guardwhere -> join -> attempts -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-stateful: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> profile(session) -> control floor -> tx-state -> wire session-state denylist -> join -> attempt -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-transaction: D{sizecap(buffer) -> split + all-statements gate -> sizecap(statement) -> Classify -> readeranalysis -> profile(session) -> control floor -> join -> attempt -> wireControl -> profile(session) -> control floor -> ParseTxControl -> handleTxControl}
  wire-extended/Parse-ordinary: R{segment read-only wrap acquired at entry} -> D{sizecap -> Classify; empty/control branches away -> readeranalysis -> authorizeunit -> profile(session) -> guardwhere -> statement resource reservation -> target Parse}
  wire-extended/Parse-control: R{segment read-only wrap acquired at entry} -> D{sizecap -> Classify -> deferred control: reserve/store only; admission waits for Execute}
  wire-extended/Execute-ordinary: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy -> authorizeunit -> tx-state + require segment wrap -> attempt -> ExecuteOp}
  wire-extended/Execute-deferred-control: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy -> release segment wrap -> wireControl -> profile(session) -> control floor -> stateful: tx-state/session-state/attempt/dispatch; transaction: ParseTxControl/handleTxControl}
  wire-startup/GUC: D{authenticate -> authorize target -> profile exposure -> reserve -> pin/checkout grammar -> UTF-8 lease -> fresh policy -> admitWireSet -> target SET; repeat per GUC}
```
<!-- admission-chain-baseline-aeac553:end -->

The baseline had no per-statement `reportedgrammar` invocation. Its grammar
verification happened at PostgreSQL pool checkout. It also used direct guard
calls rather than orchestrator invocations. The intentional order change is
visible on RPC session, both simple-wire producers, and extended Parse:
`profile` now precedes `readeranalysis` and `authorizeunit`.

## Current

<!-- admission-chain-current:begin -->
```text
profile=v1compat
  pooled/ordinary: O1{sizecap} -> D{Classify} -> O2{profile(v1compat) → procedural} -> D{actual-class grant + fresh policy} -> O3{readeranalysis → guardwhere} -> D{attempt} -> O4{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}
  pooled/control: O1{sizecap} -> D{Classify} -> O2{profile(v1compat) → procedural} -> D{off-session control refusal; no later boundary}
  rpc-session/ordinary: O1{sizecap} -> D{Classify; control branches away} -> O2{profile(v1compat) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{tx-state -> attempt} -> O3{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}
  rpc-session/control-stateful: O1{sizecap} -> D{Classify -> control route} -> O2{profile(v1compat) → procedural} -> D{control floor} -> O3{authorizeunit} -> D{tx-state} -> O4{sessionstate} -> D{RPC SET admin floor -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-transaction: O1{sizecap} -> D{Classify -> control route} -> O2{profile(v1compat) → procedural} -> D{control floor -> ParseTxControl -> handleTxControl}
  wire-simple/decoded-ordinary: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{sizecap} -> D{Classify; control branches away} -> O2{profile(v1compat) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{tx-state -> attempt} -> O3{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or refusal -> decoded dispatch}
  wire-simple/decoded-control: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{sizecap} -> D{Classify -> control route} -> O2{profile(v1compat) → procedural} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{sessionstate} -> D{stateful: attempt -> per-statement read-only wrap -> decoded dispatch; transaction: ParseTxControl -> handleTxControl}
  wire-simple/raw-ordinary: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify; control branches away} -> O4{profile(v1compat) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{join -> attempts -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-stateful: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> control route} -> O4{profile(v1compat) → procedural} -> D{control floor -> tx-state} -> O5{sessionstate} -> D{join -> attempt -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-procedural: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> control route} -> O4{profile(v1compat) → procedural} -> D{control floor -> procedural branch} -> O5{readeranalysis → authorizeunit} -> D{tx-state -> join -> attempt -> raw-segment dispatch -> routine-cache invalidation}
  wire-decoded-or-extended/control-procedural: D{decoded control route, or extended Execute of a deferred control -> wireControl} -> O1{profile(v1compat) → procedural} -> D{control floor -> procedural branch} -> O2{readeranalysis → authorizeunit} -> D{tx-state -> attempt -> dispatch -> routine-cache invalidation}
  wire-simple/raw-control-transaction: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> owned-control route} -> O4{profile(v1compat) → procedural} -> D{control floor -> join -> attempt -> wireControl} -> O5{profile(v1compat) → procedural} -> D{control floor -> ParseTxControl -> handleTxControl}
  wire-extended/Parse-ordinary: R{segment read-only wrap acquired at entry} -> O1{reportedgrammar} -> O2{sizecap} -> D{Classify; empty/control branches away} -> O3{profile(v1compat) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{statement resource reservation -> target Parse}
  wire-extended/Parse-control: R{segment read-only wrap acquired at entry} -> O1{reportedgrammar} -> O2{sizecap} -> D{Classify -> deferred control: reserve/store only; admission waits for Execute}
  wire-extended/Execute-ordinary: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{reportedgrammar} -> O2{authorizeunit} -> D{tx-state + inspect segment wrap} -> O3{readonlyenforcement if reader wrap absent} -> D{attempt -> ExecuteOp}
  wire-extended/Execute-deferred-control: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{reportedgrammar} -> D{release segment wrap -> wireControl} -> O2{profile(v1compat) → procedural} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{sessionstate} -> D{stateful: attempt -> dispatch; transaction: ParseTxControl -> handleTxControl}
  wire-startup/GUC: D{authenticate -> authorize target -> exposure -> reserve -> pin/checkout grammar -> UTF-8 lease -> fresh policy} -> O1{sessionstate} -> D{target SET; repeat per GUC}
profile=session
  pooled/ordinary: O1{sizecap} -> D{Classify} -> O2{profile(session) → procedural} -> D{actual-class grant + fresh policy} -> O3{readeranalysis → guardwhere} -> D{attempt} -> O4{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}
  pooled/control: O1{sizecap} -> D{Classify} -> O2{profile(session) → procedural} -> D{off-session control refusal; no later boundary}
  rpc-session/ordinary: O1{sizecap} -> D{Classify; control branches away} -> O2{profile(session) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{tx-state -> attempt} -> O3{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}
  rpc-session/control-stateful: O1{sizecap} -> D{Classify -> control route} -> O2{profile(session) → procedural} -> D{control floor} -> O3{authorizeunit} -> D{tx-state} -> O4{sessionstate} -> D{RPC SET admin floor -> attempt -> per-statement read-only wrap -> dispatch}
  rpc-session/control-transaction: O1{sizecap} -> D{Classify -> control route} -> O2{profile(session) → procedural} -> D{control floor -> ParseTxControl -> handleTxControl}
  wire-simple/decoded-ordinary: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{sizecap} -> D{Classify; control branches away} -> O2{profile(session) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{tx-state -> attempt} -> O3{readonlyenforcement if target capability absent} -> D{per-statement read-only wrap or refusal -> decoded dispatch}
  wire-simple/decoded-control: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{sizecap} -> D{Classify -> control route} -> O2{profile(session) → procedural} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{sessionstate} -> D{stateful: attempt -> per-statement read-only wrap -> decoded dispatch; transaction: ParseTxControl -> handleTxControl}
  wire-simple/raw-ordinary: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify; control branches away} -> O4{profile(session) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{join -> attempts -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-stateful: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> control route} -> O4{profile(session) → procedural} -> D{control floor -> tx-state} -> O5{sessionstate} -> D{join -> attempt -> raw-segment read-only wrap -> dispatch}
  wire-simple/raw-control-procedural: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> control route} -> O4{profile(session) → procedural} -> D{control floor -> procedural branch} -> O5{readeranalysis → authorizeunit} -> D{tx-state -> join -> attempt -> raw-segment dispatch -> routine-cache invalidation}
  wire-decoded-or-extended/control-procedural: D{decoded control route, or extended Execute of a deferred control -> wireControl} -> O1{profile(session) → procedural} -> D{control floor -> procedural branch} -> O2{readeranalysis → authorizeunit} -> D{tx-state -> attempt -> dispatch -> routine-cache invalidation}
  wire-simple/raw-control-transaction: O1{reportedgrammar} -> O2{sizecap} -> D{split + all-statements gate} -> O3{sizecap} -> D{Classify -> owned-control route} -> O4{profile(session) → procedural} -> D{control floor -> join -> attempt -> wireControl} -> O5{profile(session) → procedural} -> D{control floor -> ParseTxControl -> handleTxControl}
  wire-extended/Parse-ordinary: R{segment read-only wrap acquired at entry} -> O1{reportedgrammar} -> O2{sizecap} -> D{Classify; empty/control branches away} -> O3{profile(session) → procedural → readeranalysis → authorizeunit → guardwhere} -> D{statement resource reservation -> target Parse}
  wire-extended/Parse-control: R{segment read-only wrap acquired at entry} -> O1{reportedgrammar} -> O2{sizecap} -> D{Classify -> deferred control: reserve/store only; admission waits for Execute}
  wire-extended/Execute-ordinary: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{reportedgrammar} -> O2{authorizeunit} -> D{tx-state + inspect segment wrap} -> O3{readonlyenforcement if reader wrap absent} -> D{attempt -> ExecuteOp}
  wire-extended/Execute-deferred-control: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{reportedgrammar} -> D{release segment wrap -> wireControl} -> O2{profile(session) → procedural} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{sessionstate} -> D{stateful: attempt -> dispatch; transaction: ParseTxControl -> handleTxControl}
  wire-startup/GUC: D{authenticate -> authorize target -> exposure -> reserve -> pin/checkout grammar -> UTF-8 lease -> fresh policy} -> O1{sessionstate} -> D{target SET; repeat per GUC}
```
<!-- admission-chain-current:end -->

The current block deliberately shows repeated stages when production performs
separate evaluations. In particular, raw simple Query checks `sizecap` once for
the whole buffer and again for each split statement; raw transaction controls
hit `profile` during gate/join and again in `wireControl`; extended Execute
re-runs `reportedgrammar` and `authorizeunit` instead of inheriting Parse's
answers. Extended controls are stored at Parse and admitted only at Execute.
