# Admission gate matrix — surface × refusal identity

The admission pipeline's normative inventory: every refusal sentinel the
engine's gate layers can produce, and where each is reachable — which
surface, at which layer, with which preconditions. It is written FROM the
code (every row cites `file:line` at the baseline it was measured), and
`core/exec/gate_matrix_test.go` walks the code's sentinel declarations
against this document's rows, so a sentinel added without a row fails the
build — the matrix cannot silently fall behind the code.

This is the specification the phase-1 refactor's behaviour-preservation
cells implement: for each row, the same input must produce the same refusal
identity after the guards move behind the pipeline. Asymmetries between
surfaces are STATED here as decisions, with the reason, so they read as
rulings rather than bugs.

Surfaces (the `onSession` axis of the profile gate, plus the frame-level
splits the extended protocol imposes):

| surface | entry point | physical context |
|---|---|---|
| **pooled** | `engine.go run` — `Execute`/`ExecuteStream`/`RunScript` per split statement | a pooled connection; `pinned` only when a session transaction is open |
| **session** | `SessionExecute` → `executeSessionUnit` (+ `tokenControl` for control verbs) | an RPC session; pooled backend unless a transaction is pinned |
| **wire simple** | `WireQuery` → `gateWireStatement` per split statement | a front-door wire session on one pinned backend |
| **wire extended** | `WireParse` (gate at Parse) + `WireExecutePortal` (re-authorize at every Execute) | same pinned backend; segments between Sync frames |

Layer meanings: **intake** = the size bounds that precede understanding;
**classify** = the lexer's shape verdicts; **capability** = the profile
gate; **reader** = the editors-first reader stage; **guard** = the WHERE
guard; **session-state** = the SET/LOCK/Lifecycle gates; **authority** =
grant/role floors and the transaction-state machine; **resource** = object
namespace and budget caps; **transport** = wire-level sequencing and
delivery.

> **The `raised at` coordinates in sections 1–5 are generated, not typed.** They
> are derived from `core/exec`'s own syntax tree, using the same rule the
> conformance walk applies: a cited raise line must carry an identifier naming
> one of the row's sentinels. Keeping them by hand did not work — the change
> that makes an update necessary is usually the change that moves the lines, so
> a number read at the start of an edit is wrong by the end of it.
>
> After moving code in `core/exec`, run:
>
> ```
> go run ./internal/gatematrix/cmd/coordgen -pkg ./core/exec -doc docs/admission-gate-matrix.md
> ```
>
> `-check` reports staleness without writing. Every other cell in a row is prose
> written by a person and is never touched.

## 1. Classification and shape (every surface, identity uniform)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrScriptTooLarge` | decl classify.go:61; admission_stages.go:124, admission_stages.go:125, admission_stages.go:569 | all four | intake | Before classification everywhere, so the audit record equals what ran. |
| `ErrEmptyStatement` | decl classify.go:45; classify.go:598, script.go:116, script.go:54, txcontrol.go:105, txcontrol.go:91, wire_extended.go:221, wire_query.go:312, wire_query.go:315 | pooled, session (via `RunScript`/`Classify`); **tolerated on wire simple** (wire_query.go:312 and wire_query.go:315 answer EmptyQueryResponse) and **legal on wire ext** (WireParse's empty arm, wire_extended.go:221, charged like any object) | classify | **The stated asymmetry**: raised on the token paths, deliberately tolerated on the wire paths — empty scripts are guarded UI-side (design ruling, 2026-09-10) and PostgreSQL itself answers an empty Query with EmptyQueryResponse. A wire client's validation probe must not die (the pgjdbc lesson, v0.3.x).  Justification: divergence:§6.1. |
| `ErrMalformedStatement` | decl classify.go:54; classify.go:368, classify.go:595, classify.go:665, classify.go:683, session_state.go:166, txcontrol.go:386, txcontrol.go:500, txcontrol.go:564 | all four | classify | Identity uniform; the wire simple path frames it as a gate refusal. |
| `ErrMultiStatement` | decl classify.go:48; classify.go:288, txcontrol.go:482, txcontrol.go:505, txcontrol.go:536 | pooled, session (`Execute` is one-statement; `RunScript` splits instead) ; wire simple SPLITS and gates each part; wire ext is inherently one statement per Parse | classify | The simple-wire path's split is the A10 all-or-nothing fan-out: a refused statement anywhere refuses the whole buffer.  Justification: divergence:§6.7 — one rule, two expressions: refuse the text on pooled/session Execute, split-and-gate on RunScript and the wire simple path.. |

## 2. Capability and profile (surface answers can differ — by design of `onSession`)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrStatementUnsupported` | decl classify.go:52; admission_stages.go:288, admission_stages.go:492, admission_stages.go:496, admission_stages.go:571, classify.go:517, classify.go:573, classify.go:609, profile.go:104, profile.go:124, profile.go:127, profile.go:138, profile.go:74, profile.go:85, session_state.go:124, session_state.go:138, session_state.go:427, session_state.go:431, session_tx.go:118, txcontrol.go:397, txcontrol.go:97 | all four (each via its own admit site) | capability (+ classify shape arms) | The v1compat arms (control, data-modifying CTE) and the session-profile arms (control off-session, pending-control verbs, no-admissible-form verbs) are uniform text per arm. **One phase-1 ordering decision changes two collision classes on migrated session paths**: a read-only v1compat data-modifying CTE with a UDF moves from `ErrReaderAdvancedPattern` to `ErrStatementUnsupported`; without a UDF it moves from `auth.ErrDenied` to `ErrStatementUnsupported`. Profile admissibility is deliberately first in both cases. **The procedural arm is a PLACEMENT refusal, not a capability one**: `Profile.admit` admits DO and CALL (they have an admissible form), and the procedural stage refuses them off the wire, where the backend goes back to a pool and an opaque body's session state would be inherited by the next caller. Same sentinel, different question — see applicability:§7.7. Justification: divergence:§6.3 and §6.5 — profile ordering (§6.3) and onSession control verbs (§6.5). |
| `ErrNoWhere` | decl classify.go:57; admission_stages.go:570, profile.go:225, profile.go:233 | all four | guard | Uniform text both arms. |
| `ErrReaderAdvancedPattern` | decl reader_analysis.go:65; admission_stages.go:572, reader_analysis.go:111, reader_analysis.go:113, reader_analysis.go:90 | pooled, session, wire simple, wire ext (at Parse) | reader | Reader units only (`pol.ReadOnly`); needs a PostgreSQL-family target (`HasRoutineCatalog`). A catalog-read failure is an operational admission error, not this policy-refusal identity. Justification: divergence:§6.3 — the ordering flip; reader-stage reach itself is all-surface. |
| `ErrReadOnlyUnenforceable` | decl unitpolicy.go:104; admission_stages.go:579, admission_stages.go:67, admission_stages.go:68 | pooled and internal session (audited-and-ran, NOT refused — see notes), wire simple/ext | authority (enforcement) | **physical-surface asymmetry, decided**: on RPC/TUI surfaces a non-transactional target runs under classifier enforcement only, with a `readonly_unenforced` audit line — refusing would take the reader role away from every SQLite-class target for a guarantee never offered there. The physical wire arm refuses (fail closed) because the front door promises server-enforced read-only; an unset or unknown physical context also refuses rather than assuming the guarantee was not promised. A failure while opening an otherwise supported transaction is operational and remains opaque. Justification: divergence:§6.2. |

## 3. Session-state gates (SET / LOCK / RESET — pooled allowlist vs wire denylist)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrSetGUCRefused` | decl session_state.go:33; admission_stages.go:508, admission_stages.go:573, session_state.go:201, session_state.go:205, session_state.go:216 | session (pooled-backend allowlist); stateless pooled is unreachable | session-state | Pooled-backend arm of the GUC model.  Justification: divergence:§6.4. |
| `ErrSetNotLocal` | decl session_state.go:30; admission_stages.go:510, admission_stages.go:574, session_state.go:212 | session; stateless pooled is unreachable | session-state | SET without LOCAL would leak to the next pool user.  Justification: divergence:§6.4. |
| `ErrSetOutsideTx` | decl session_state.go:39; admission_stages.go:512, admission_stages.go:575, session_state.go:220, session_state.go:406 | session, wire simple + ext; stateless pooled is unreachable | session-state | The wire arm keeps the same identity for the analogous denylist rule.  Justification: divergence:§6.4 — arms on both GUC models. |
| `ErrWireSetRefused` | decl session_state.go:363; admission_stages.go:516, admission_stages.go:577, session_state.go:372, session_state.go:376, session_state.go:382, session_state.go:386, session_state.go:391, session_state.go:448 | wire simple + wire ext (via `admitSessionState`) | session-state | Wire-only: the backend is discarded at close, so the pooled leak hazard does not exist and the denylist replaces the allowlist. **Two GUC models stay distinct — flattening them is a phase-1 mutation cell.**  Justification: divergence:§6.4 — the wire denylist arm. |
| `ErrLockOutsideTx` | decl session_state.go:44; admission_stages.go:514, admission_stages.go:576, session_state.go:233 | session, wire simple + ext; stateless pooled is unreachable | session-state | A lock outside a transaction releases immediately — admitting it would be a lie.  Justification: applicability:§7.6 — reachable only on session-shaped calls via admitSessionState. |
| `ErrGrammarDrifted` | decl dialect.go:150; admission_stages.go:580, admission_stages.go:89, dialect.go:181, dsn.go:151 | pooled (checkout), wire (reported-parameter re-check, A26) | session-state | Currently raised at acquisition; the phase-1 A26 fold extends the re-read to per-statement on a pinned session.  Justification: divergence:§6.6 — raised at pool checkout today; the per-statement pinned re-read lands with the A26 fold.. |

## 4. Authority and transaction state (session machinery)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `auth.ErrDenied` | the read floor (engine.go:502, session_engine.go:51), canonical class-floor rule (wire_execute.go:282 and wire_execute.go:289, invoked only by `authorizeUnitStage`), control floors (wire_query.go:221, wire_query.go:224, wire_execute.go:197, wire_execute.go:205) | all four | authority | The uniform denial — never discloses existence. Mapped to `CodeDenied`/SQLSTATE 42501-family per surface renderer. A profile-invalid statement is refused earlier as `ErrStatementUnsupported`; §6.3 records the deliberate capability disclosure this creates for an otherwise-ungranted class. |
| `ErrTxAborted` | decl session_tx.go:45; session_engine.go:190, session_tx.go:337, wire_execute.go:174, wire_execute.go:231, wire_execute.go:256, wire_extended.go:873, wire_query.go:246, wire_query.go:257, wire_query.go:280 | session, wire simple, wire ext | authority | Failed-transaction state; recovery controls only.  Justification: applicability:§7.5. |
| `ErrTxAlreadyOpen` | decl session_tx.go:36; session_tx.go:131 | session, wire (both via `handleTxControl`) | authority | BEGIN on an open transaction.  Justification: applicability:§7.5. |
| `ErrNoOpenTx` | decl session_tx.go:39; session_tx.go:331 | session, wire | authority | COMMIT/ROLLBACK with nothing to finish.  Justification: applicability:§7.5. |
| `ErrNoSuchTx` | decl txstatus.go:36; txstatus.go:83, txstatus.go:88 | session, wire, RPC status | authority | A transaction id with no progression — "not the caller's" answers exactly as "never existed", so tx.status cannot be used to discover which transaction ids exist.  Justification: applicability:§7.4. |
| `ErrTxChainUnsupported` | decl session_tx.go:57; session_tx.go:107 | session, wire | authority | e.g. ROLLBACK followed by BEGIN in one buffer shape; both boundaries must be recorded.  Justification: applicability:§7.5. |
| `ErrTxControlUnsupported` | decl txcontrol.go:71; txcontrol.go:265, txcontrol.go:360 | session, wire | authority | A transaction-control shape the machine does not own (WITH CONSISTENT SNAPSHOT and kin).  Justification: applicability:§7.5. |
| `ErrTxAuthorityChanged` | decl session_tx.go:52; session_engine.go:154, session_engine.go:157, wire_execute.go:90, wire_execute.go:93 | session, wire | authority | Demotion mid-session; rollback cleanup failed or the transaction's authority ended.  Justification: applicability:§7.5. |
| `ErrSessionNotFound` | decl session.go:62; session.go:722, session_engine.go:138, wire_execute.go:80, wire_extended.go:68 | session, wire | authority | A close raced the call, or the id is not the caller's.  Justification: applicability:§7.5. |
| `ErrSessionBusy` | decl session.go:56; session.go:826, session_timeout.go:326, session_tx.go:611 | session, wire | authority | One statement at a time per session; a second concurrent call refuses.  Justification: applicability:§7.5. |
| `ErrConnectionDraining` | decl session.go:72; conns.go:114, conns.go:69, dial_failed.go:391, session.go:495 | all four | authority | Connection deleted/closed mid-call. |
| `ErrSessionCapExceeded` / `ErrLeaseCapExceeded` / `ErrResidentBudgetExceeded` | decl session.go:67, session.go:454 and session.go:457; scheduler.go:347, session.go:499, session.go:503, session.go:507, session.go:518, session.go:527, wire_session.go:400, wire_session.go:408, wire_session.go:410 | session/wire open | resource | Cap refusals at open; audited distinctly. Uniform on the wire EXCEPT where the refusal is raised after both the credential and the grant have been checked: those carry a post-authorization witness and render 53300 with a fixed message, so an authorized developer is told the system is full rather than that their credential is wrong (front-door rows carry the identity).  Justification: applicability:§7.2； applicability:§7.2； applicability:§7.2. |
| `ErrWorkspaceNotFound` | decl workspaces.go:23; workspaces.go:104, workspaces.go:129, workspaces.go:80 | RPC admin surface only | authority | Not a gate refusal — listed for walk completeness (it is a public sentinel).  Justification: applicability:§7.3. |
| `ErrConnectionNameTaken` | decl conns.go:38; conns.go:381 | RPC admin surface only | resource | Connection creation collided with an existing name. Not a statement-gate refusal — listed for walk completeness and mapped publicly so callers can correct the name. Justification: applicability:§7.3. |
| `ErrConnectionHasHistory` | decl conns.go:39; conns.go:603 | RPC admin surface only | resource | Connection deletion would orphan recorded history. Not a statement-gate refusal — listed for walk completeness and mapped publicly so callers can retain the referenced connection. Justification: applicability:§7.3. |
| `ErrQueueTimeout` | decl acquire_queue.go:49; scheduler.go:245, scheduler.go:247, scheduler.go:251, wire_session.go:389 | session and wire open, after the request has joined the line; the stateless pooled path holds no session and never waits | resource | A request that waited its turn for a lease and was not reached within the fixed ninety-second server wait. Deliberately not a plain capacity refusal: the instance took the request, held it in line and ran out of wait, which is a pressure signal rather than a rejection. It also carries the cap that blocked it, because an operator's remedy for a full target pool differs from one for a full session cap. Raised after the caller's credential is verified and rendered after authorization, charged as capacity, so a full instance is never disclosed as a bad credential.  Justification: applicability:§7.8. |
| `ErrAllCapacityInTransaction` | decl acquire_queue.go:62; scheduler.go:120, wire_session.go:402 | session and wire open, before the request joins the line; the stateless pooled path holds no session and never waits | resource | Every lease on the target is held by a transaction still inside its bounds, so nothing is going to be released and the wait would end ninety seconds later with the answer already available. A transaction past its bound does NOT count: that lease is going to be reclaimed, so capacity is coming and the request waits for it. Raised only before the request enters the line and never as a relabel, because a record claiming a request never waited has to be true of that request.  Justification: applicability:§7.8. |
| `ErrTargetGone` | decl acquire_queue.go:72; scheduler.go:404, wire_session.go:404 | session and wire open, while waiting in line; the stateless pooled path holds no session and never waits | resource | The connection a request was waiting for was removed by an operator. Answered at the moment of removal rather than by letting the wait expire: a timeout invites a retry that can now never succeed, and would leave the line able to admit a session onto a connection being torn down.  Justification: applicability:§7.8. |
| `ErrEngineClosing` | decl acquire_queue.go:82; scheduler.go:360, wire_session.go:406 | session and wire open, in line or arriving during shutdown; the stateless pooled path holds no session and never waits | resource | The instance began shutting down while the request was in line. Waking every waiter at the start of shutdown stops a wait outliving serving, and stops a release landing in that window from admitting a session onto pools that are already closing.  Justification: applicability:§7.8. |

## 5. Extended-protocol objects and transport

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrExtendedUnsupportedTarget` | decl wire_extended.go:148; wire_extended.go:81 | wire ext | capability | Extended frames on a non-PostgreSQL target: refused loudly, never approximated.  Justification: applicability:§7.1. |
| `ErrDuplicateStatement` / `ErrDuplicatePortal` | decl wire_extended_objects.go:26 and wire_extended_objects.go:63; wire_extended_objects.go:472, wire_extended_objects.go:529 | wire ext | resource | Bind/Parse naming a live named object.  Justification: applicability:§7.1； applicability:§7.1. |
| `ErrUnknownStatement` / `ErrUnknownPortal` | decl wire_extended_objects.go:49 and wire_extended_objects.go:67; wire_extended_objects.go:515, wire_extended_objects.go:554 | wire ext | resource | Phantom name — the store keeps the record when in doubt (the 42P05 lesson).  Justification: applicability:§7.1； applicability:§7.1. |
| `ErrNamedObjectCap` | decl wire_extended_objects.go:39; wire_extended_objects.go:489, wire_extended_objects.go:533 | wire ext | resource | 256 statements / 64 portals per session.  Justification: applicability:§7.1. |
| `ErrParamCap` | decl wire_extended_objects.go:45; wire_extended.go:359 | wire ext | intake | Bind > 8192 params.  Justification: applicability:§7.1. |
| `ErrPendingCloseCap` | decl wire_extended_objects.go:59; wire_extended.go:457 | wire ext | resource | A Close whose recovery obligation cannot be tracked — capacity refuses rather than discarding the obligation.  Justification: applicability:§7.1. |
| `ErrRetainedBudget` | decl wire_extended_objects.go:35; wire_extended_objects.go:701 | wire ext | resource | Retained-state reservation failure; nothing forwarded.  Justification: applicability:§7.1. |
| `ErrWireEmitNil` | decl wire_query.go:93; wire_extended.go:527, wire_extended.go:638, wire_extended.go:782, wire_query.go:143 | wire ext (engine API) | transport | Programming-error guard on the emit callback.  Justification: applicability:§7.1. |
| `ErrWireFaceLost` | decl wire_query.go:913; wire_query.go:411, wire_query.go:928 | wire simple + ext | transport | The pinned connection's raw face is gone; session closes.  Justification: applicability:§7.1. |
| `ErrWireSequenceRefused` | decl wire_query.go:925; session_tx.go:221, wire_extended.go:115, wire_extended.go:832, wire_query.go:457, wire_query.go:574, wire_query.go:581 | wire simple + ext | transport | A message or hidden transaction setup the pinned wire cannot accept while an extended-query segment is open — end it with Sync first.  Justification: applicability:§7.1. |
| `ErrNotExecuted` | decl wire_query.go:193; wire_extended.go:1283, wire_query.go:386, wire_query.go:647 | wire simple + ext | transport | The recorded OUTCOME of a statement PostgreSQL did not run because an earlier one failed — a history status, not a client refusal.  Justification: applicability:§7.1. |
| `ErrDecodedResultTruncated` | decl wire_query.go:901; wire_query.go:886 | wire simple (non-PostgreSQL decoded producer) | transport | A page-bound result on a target that cannot stream unbounded.  Justification: applicability:§7.1. |
| `ErrCancelKeyCollision` | decl cancel_registry.go:138; cancel_registry.go:168 | wire (engine internal) | transport | Cancel-key registry collision — CSPRNG pair uniqueness.  Justification: applicability:§7.1. |

## 6. Policy and semantic divergences, on record

A DIVERGENCE is a cross-surface difference in the answer a client can
observe, caused by a decision rather than by what physically exists. Each
one below is a ruling with its reason; the walk requires every
restricted-surface row to declare its justification category and to name
one of these entries.

1. **`ErrEmptyStatement`** — raised on token paths; tolerated on the wire (EmptyQueryResponse; empty Parse legal and charged). Decision: guard empty scripts UI-side. (Row §1.)
2. **`ErrReadOnlyUnenforceable`** — pooled and internal-session execution runs unenforced with an audit line when the target cannot host the wrap; physical wire execution and an unset/unknown physical context refuse. Decision: the promise differs by physical surface because the guarantee offered differs. (Row §2.)
3. **Profile admissibility before reader analysis and class authorization** — legacy session and wire paths ran `readerAnalysis → authorizeUnit → profile.admit`; pooled already put the profile first. **Decided (Johno, 2026-09-11; pipeline design §7.4): keep profile first everywhere.** One ordering decision therefore creates two intentional identity changes on the migrated paths. A read-only v1compat data-modifying CTE with a UDF moves from `ErrReaderAdvancedPattern` to `ErrStatementUnsupported`; one without a UDF moves from `auth.ErrDenied` to `ErrStatementUnsupported`. The second flip reveals that the profile cannot run the statement where the old flat authorization denial revealed no capability detail. That disclosure is accepted in favor of a uniform, fundamental answer: granting the class or removing the UDF still cannot make a profile-invalid statement runnable. The affected corpus classes are predicted before the flip; per-row deltas go to a ruling, never to quiet regeneration.
4. **GUC models** — pooled allowlist (`ErrSetGUCRefused`/`ErrSetNotLocal`) vs wire denylist (`ErrWireSetRefused`; `ErrSetOutsideTx` has an arm on both models). Decision: the leak hazard genuinely differs (backend discarded at close); the phase-1 chain keeps them as two applicability sets of one stage. (Rows §3.)
5. **`onSession` control verbs** — `BEGIN`/`COMMIT`/`ROLLBACK`/`SET`/`RESET`/`LOCK` are admitted on the session profile ONLY on a session (engine action, never forwarded text); off one they refuse with `ErrStatementUnsupported`. The pooled `run` passes `pinned != nil`; every other admit site passes literal `true` because the surface implies it. (Row §2.)

**Live profile downgrade (baseline invariant).** Changing `session` to `v1compat` withdraws existing wire sessions after the transactional profile update. This preserves the phase-1 baseline and prevents an open transaction from being stranded after `COMMIT` and `ROLLBACK` become profile-refused; the close rolls the target transaction back. The connection's independent exposure property remains unchanged, so a later wire session may open under the narrower capability. This withdrawal is capability cleanup, not an exposure transition.

6. **`ErrGrammarDrifted` timing** — raised at pool checkout (acquisition) today; the per-statement pinned-session re-read lands with the phase-1 A26 fold, at which point the wire surfaces gain the per-statement refusal. A deliberate, dated divergence in WHEN the identity is raised, not whether it can be. (Row §3.)
7. **`ErrMultiStatement` split vs refuse** — pooled and session refuse multi-statement text at `Execute`; `RunScript` and the wire simple path SPLIT and gate each part, all-or-nothing. One rule, two expressions: the same outcome (a refused statement anywhere refuses everything after it) reached by refusing the text on one surface and by gating each part on the others. (Row §1.)

## 7. Physical applicability boundaries, on record

An APPLICABILITY boundary is not a divergence: the frame, object namespace,
or session state that raises the identity does not exist on the other
surface, so there is no input that could reach it. Absence by construction,
not a policy choice — recorded here so a restricted surface column reads
as physics rather than as a discretionary exception. The walk requires
every restricted-surface row justified by physics to declare `applicability`
and to name one of these entries.

1. **Extended-protocol and wire-transport refusals (§5 rows)** — `ErrExtendedUnsupportedTarget`, `ErrDuplicateStatement`, `ErrDuplicatePortal`, `ErrUnknownStatement`, `ErrUnknownPortal`, `ErrNamedObjectCap`, `ErrParamCap`, `ErrPendingCloseCap`, `ErrRetainedBudget`, `ErrWireEmitNil`, `ErrWireFaceLost`, `ErrWireSequenceRefused`, `ErrNotExecuted`, `ErrDecodedResultTruncated` and `ErrCancelKeyCollision` are properties of the extended protocol's frames, of a pinned wire session, or of the wire producers — object namespace caps, portal/statement names, parameter caps, segment sequencing, delivery and face-loss identities have no meaning on the pooled or RPC-session paths, which send no such frames. `ErrNotExecuted` and `ErrDecodedResultTruncated` are wire-path OUTCOME identities (recorded history statuses / a decoded-producer page bound). `ErrCancelKeyCollision` is an engine-internal registry guard asserted by its own cells. (Rows §5.)
2. **Session-open resource caps** — `ErrSessionCapExceeded` / `ErrLeaseCapExceeded` / `ErrResidentBudgetExceeded` are raised at session OPEN (RPC session or wire session), so the pooled stateless path — which holds no session — cannot reach them. (Row §4.)
3. **RPC admin-operation identities** — `ErrWorkspaceNotFound`, `ErrConnectionNameTaken` and `ErrConnectionHasHistory` are RPC admin-surface lookup or lifecycle identities, not statement-gate refusals; listed for walk completeness. (Row §4.)
4. **`ErrNoSuchTx`** — reachable from the RPC status verbs and the wire's transaction surfaces; a stateless pooled call has no transaction id to ask about. (Row §4.)
5. **Transaction-state machinery (§4 rows)** — `ErrTxAborted`, `ErrTxAlreadyOpen`, `ErrNoOpenTx`, `ErrTxChainUnsupported`, `ErrTxControlUnsupported`, `ErrTxAuthorityChanged`, `ErrSessionNotFound`, `ErrSessionBusy` and `ErrConnectionDraining` require a session-bound or wire-bound call to be reachable: the transaction machine and the session claim exist only there, and a stateless pooled `Execute` has no transaction state to refuse against (a pooled BEGIN refuses at the capability layer instead). (Rows §4.)
6. **Session-state gates need a session-shaped call** — `ErrSetOutsideTx` and `ErrLockOutsideTx` are unreachable on the stateless pooled surface: it owns no session state, and an open pinned transaction would contradict the outside-transaction premise. They are reachable on RPC sessions and both wire protocols through `admitSessionState`; extended Parse stores the owned control and its Execute takes the wire-control route. (Rows §1, §3.)

7. **Procedural verbs need a discarded backend** — DO and CALL are admitted on a PINNED wire session only, and the stage reads `Context.PinnedBackend` rather than `PhysWire`: the transport is a proxy that drifts, because a front-door session against a target that does not speak the PostgreSQL wire protocol takes the decoded path, where statements run on a POOLED target connection and nothing is discarded at close. The body is one opaque dollar-quoted token that no lexer or AST will read, so the engine cannot know whether it contains a `SET`; a wire session's backend is pinned for the session's life and DISCARDED at close, which removes the hazard structurally rather than by inspection. A pooled connection and an RPC session's connection both outlive the caller, so the refusal stands there — the same physical distinction the §3 GUC models are drawn on. They are also NOT owned control in the extended protocol: `isOwnedControl` excludes them, so Parse sends them to the target and Execute runs them on the session's pinned backend. Treating them as owned reported success while the body ran on a pooled connection nobody could see. This is placement, NOT role: a reader is refused on every surface by `ErrReaderAdvancedPattern` and by the class floor, which the dispatch chain (`readeranalysis → authorizeunit`) composes because the control route skips the session chain. (Rows §2.)

8. **Connection-admission queue outcomes** — `ErrQueueTimeout`, `ErrAllCapacityInTransaction`, `ErrTargetGone` and `ErrEngineClosing` are outcomes of the session-admission line in scheduler.go, reachable only by a request asking for a SESSION: an RPC session open or a wire session open. The stateless pooled path holds no session, takes no lease and never joins the line, so no input on that surface can reach them — the same construction as the session-open caps in §7.2, of which this line is the waiting half. Each is a refusal of CAPACITY and never of credential: all four are raised after the caller's credential is verified and are disclosed after authorization, so an authorized developer is told the instance is full, the target is gone, or the instance is closing rather than being handed the uniform denial that reads as a wrong password. (Rows §4.)

## 8. Walk obligations (enforced by core/exec/gate_matrix_test.go)

- Every sentinel `errors.New`-declared in `core/exec` non-test code has a
  row in the §1–§5 inventory tables (row-scoped: membership means a row,
  never a prose mention), or an explicit `walk-exempt` entry in the test
  with the reason.
- Every row's coordinates are re-verified against the live tree: the
  DECLARATION anchor within a drift window, and every RAISE site
  structurally — the named identifier must appear on that line of that
  file, parsed from the AST, so an invented or stale locator is a red walk.
- Every restricted-surface row declares its justification — `divergence`
  (§6, a decision with an observable consequence) or `applicability`
  (§7, physics: the raising frame does not exist elsewhere) — and the
  named on-record entry must carry the sentinel.

## 9. Corpus prediction for the phase-1 profile-ordering flips

(Recorded BEFORE any drive changes; see divergence §6.3.)

One intentional ordering decision moves both reader analysis and class
authorization after the profile gate on the migrated session paths. It creates
two client-visible identity changes for read-only v1compat data-modifying CTEs:
with a UDF, the reader stage's identity changes to the profile's; without a UDF,
the class authorization identity changes to the profile's.

**The prediction: the manifest delta is EMPTY, and it is empty by
construction, not by luck.** The corpus replay's gate decision
(`gateDecision` in the corpus test: profile admit, then the WHERE guard)
never runs reader analysis OR class authorization, so neither profile-ordering
change can reach the manifest. The one `refused:nested-mutation`
row in the committed manifest (000005_update_backfill_contacts.sql,
ordinal 3, verb UPDATE) refuses at the profile gate in BOTH orders and keeps
its identity.

**Therefore, at the Step that flips the wire paths:** a changed corpus
manifest has no third story available — it is a DEFECT, enumerated per row
(old identity, new identity) and surfaced for a ruling; it is not a
regeneration and not a carve-out. The structural reason is asserted by a
cell (`TestGateMatrix_CorpusReplayCannotSeeLaterAdmissionStages`), which fails
if the replay ever grows a reader-analysis or class-authorization arm, because
at that point the prediction above goes stale and must be re-derived and
re-recorded BEFORE the corpus runs again.

## 10. Matrix-driven behavioral cells

This table is executable input to `TestGateMatrix_BehavioralSurfaceCells`, not
illustrative prose. Case IDs are stable review handles. Every surface cell is
explicit: `pass`, `refuse:<sentinel>`, or `unreachable:<physical reason>`.
`unreachable` is never treated as a pass.

The suite is deliberately exhaustive over the refusal identities declared by
the live admission-stage registry (`RegisteredAdmissionCodes`) and adds the
empty-statement classification divergence plus positive controls. It drives the
real classifier, split lexer, stage adapters, reason-to-sentinel mapping, and
the production pooled/session admission helpers without opening a target
connection. Wire simple and wire extended share the production session chain;
their separate columns remain explicit because their transport front ends,
including empty-statement handling, differ.

| case ID | mode | SQL | profile | policy | tx open | pooled | session | wire simple | wire extended |
|---|---|---|---|---|---|---|---|---|---|
| `A27-CONTROL-001` | statement | `SELECT 1` | session | editor | false | pass | pass | pass | pass |
| `A27-INTAKE-001` | intake | `SELECT 123456789` | session | editor | false | refuse:ErrScriptTooLarge | refuse:ErrScriptTooLarge | refuse:ErrScriptTooLarge | refuse:ErrScriptTooLarge |
| `A27-CLASSIFY-001` | empty | `<empty>` | session | editor | false | refuse:ErrEmptyStatement | refuse:ErrEmptyStatement | pass | pass |
| `A27-PROFILE-001` | profile | `BEGIN` | v1compat | editor | false | refuse:ErrStatementUnsupported | refuse:ErrStatementUnsupported | refuse:ErrStatementUnsupported | refuse:ErrStatementUnsupported |
| `A27-CONTROL-002` | profile | `BEGIN` | session | editor | false | refuse:ErrStatementUnsupported | pass | pass | pass |
| `A27-READER-001` | statement | `SELECT app.write_a_row()` | session | reader | false | refuse:ErrReaderAdvancedPattern | refuse:ErrReaderAdvancedPattern | refuse:ErrReaderAdvancedPattern | refuse:ErrReaderAdvancedPattern |
| `A27-AUTHORITY-001` | statement | `UPDATE t SET a = 1 WHERE id = 1` | session | reader | false | refuse:auth.ErrDenied | refuse:auth.ErrDenied | refuse:auth.ErrDenied | refuse:auth.ErrDenied |
| `A27-GUARD-001` | statement | `UPDATE t SET a = 1` | session | editor | false | refuse:ErrNoWhere | refuse:ErrNoWhere | refuse:ErrNoWhere | refuse:ErrNoWhere |
| `A27-STATE-001` | control | `SET statement_timeout = '1s'` | session | editor | true | unreachable:stateless-calls-have-no-session-state | refuse:ErrSetNotLocal | pass | pass |
| `A27-STATE-002` | control | `SET LOCAL search_path = public` | session | editor | true | unreachable:stateless-calls-have-no-session-state | refuse:ErrSetGUCRefused | pass | pass |
| `A27-STATE-003` | control | `SET LOCAL search_path = public` | session | reader | true | unreachable:stateless-calls-have-no-session-state | refuse:auth.ErrDenied | refuse:ErrWireSetRefused | refuse:ErrWireSetRefused |
| `A27-STATE-004` | control | `SET LOCAL statement_timeout = '1s'` | session | editor | false | unreachable:no-pinned-transaction-can-be-outside-a-transaction | refuse:ErrSetOutsideTx | refuse:ErrSetOutsideTx | refuse:ErrSetOutsideTx |
| `A27-STATE-005` | control | `LOCK TABLE t` | session | editor | false | unreachable:no-pinned-transaction-can-be-outside-a-transaction | refuse:ErrLockOutsideTx | refuse:ErrLockOutsideTx | refuse:ErrLockOutsideTx |
| `A27-GRAMMAR-001` | reported-grammar | `SELECT 1` | session | editor | false | refuse:ErrGrammarDrifted | unreachable:rpc-sessions-have-no-reported-parameter-state | refuse:ErrGrammarDrifted | refuse:ErrGrammarDrifted |
| `A27-ENFORCEMENT-001` | enforcement | `SELECT 1` | session | reader | false | pass | pass | refuse:ErrReadOnlyUnenforceable | refuse:ErrReadOnlyUnenforceable |

### Behavioral coverage boundary

This is the smallest non-lying matrix integration, not a 43 × surface live
PostgreSQL product. It exhausts the composable SQL-admission refusal codes and
checks representative classification behavior. The following inventory rows
are physically outside this unit seam and retain their dedicated tests:

- transaction/session lifecycle (`ErrTx*`, `ErrSession*`,
  `ErrConnectionDraining`) requires a claimed live session and state-machine
  progression;
- session-open and admin identities require session/resource accounting or an
  RPC lifecycle operation;
- extended object and transport identities require Parse/Bind/Execute object
  state, target frames, delivery failure, or cancel-registry state;
- read-only transaction establishment remains enforcement after the durable
  attempt; the registered enforcement stage decides only the no-capability
  refusal, and `A27-ENFORCEMENT-001` covers its physical surface split;
- remaining classifier shape identities are covered exhaustively by the
  classifier/split suites. `A27-CLASSIFY-001` is included here because its
  cross-surface divergence is an explicit matrix ruling.

The structural inventory and coordinate walks in §8 remain unchanged. The
behavioral suite is an additional obligation: deleting one surface's gate can
leave every declaration and raise coordinate intact, but must now disagree
with that surface's committed behavioral cell.
