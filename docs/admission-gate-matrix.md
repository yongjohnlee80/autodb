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

## 1. Classification and shape (every surface, identity uniform)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrScriptTooLarge` | decl classify.go:61; raised engine.go:487 (pooled), wire_execute.go:121 (session), wire_query.go:197 and wire_query.go:272 (wire simple), wire_extended.go:173 (wire ext, at Parse) | all four | intake | Before classification everywhere, so the audit record equals what ran. |
| `ErrEmptyStatement` | decl classify.go:45; classify.go:598 (empty script), script.go:54 (empty split part) | pooled, session (via `RunScript`/`Classify`); **tolerated on wire simple** (wire_query.go:275-282 answers EmptyQueryResponse) and **legal on wire ext** (WireParse's empty arm, wire_extended.go:176-192, charged like any object) | classify | **The stated asymmetry**: raised on the token paths, deliberately tolerated on the wire paths — empty scripts are guarded UI-side (design ruling, 2026-09-10) and PostgreSQL itself answers an empty Query with EmptyQueryResponse. A wire client's validation probe must not die (the pgjdbc lesson, v0.3.x).  Justification: divergence:§6.1. |
| `ErrMalformedStatement` | decl classify.go:54; classify.go:368/595/665 (unterminated comment/quote/region) | all four | classify | Identity uniform; the wire simple path frames it as a gate refusal. |
| `ErrMultiStatement` | decl classify.go:48; classify.go:288 (content after top-level `;`), txcontrol.go:482 | pooled, session (`Execute` is one-statement; `RunScript` splits instead) ; wire simple SPLITS and gates each part; wire ext is inherently one statement per Parse | classify | The simple-wire path's split is the A10 all-or-nothing fan-out: a refused statement anywhere refuses the whole buffer.  Justification: divergence:§6.7 — one rule, two expressions: refuse the text on pooled/session Execute, split-and-gate on RunScript and the wire simple path.. |

## 2. Capability and profile (surface answers can differ — by design of `onSession`)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrStatementUnsupported` | decl classify.go:52; profile.go:69 `Profile.admit` (7 call sites); classify.go:517/573/609 (shape refusals the classifier itself makes: nested data-modifying verb as the whole script's verdict at 517, PRAGMA-shaped and unclassifiable tokens at 573/609); session_state.go (RESET on pooled session, unknown SET form); unitpolicy.go fail-closed unknown profile wraps it | all four (each via its own admit site) | capability (+ classify shape arms) | The v1compat arms (control, data-modifying CTE) and the session-profile arms (control off-session, pending-control verbs, no-admissible-form verbs) are uniform text per arm. **The phase-1 ordering ruling changes ONE cross-surface answer**: a read-only v1compat unit whose data-modifying CTE also calls a user-defined function moves from `ErrReaderAdvancedPattern` (wire paths, reader first) to `ErrStatementUnsupported` (profile first) — decided, matrix §7.4 of the pipeline design.  Justification: divergence:§6.3 and §6.5 — ordering (§6.3) and onSession control verbs (§6.5)； divergence:§6.3 — the ordering flip; reader-stage reach itself is all-surface； divergence:§6.3 and §6.5 — ordering (§6.3) and onSession control verbs (§6.5). |
| `ErrNoWhere` | decl classify.go:57; profile.go:203 (top-level UPDATE/DELETE), :211 (nested mutation without WHERE at depth) | all four | guard | Uniform text both arms. |
| `ErrReaderAdvancedPattern` | decl reader_analysis.go:64; reader_analysis.go:91 (qualified UDF call), :93 (bare UDF call), :110 (DO/CALL verb), :122 (catalog unreadable — operational, still this identity) | pooled, session, wire simple, wire ext (at Parse) | reader | Reader units only (`pol.ReadOnly`); needs a PostgreSQL-family target (`HasRoutineCatalog`). The catalog-unreadable arm (:122) is the operational-failure identity — distinct from the three policy arms in meaning, same sentinel.  Justification: divergence:§6.3 — the ordering flip; reader-stage reach itself is all-surface. |
| `ErrReadOnlyUnenforceable` | decl unitpolicy.go:104; unitpolicy.go:167 (pooled wrap on a target with no TxBeginner, session profile), wire_extended.go:797 (execute-scoped: reader whose wrap is absent mid-segment — demotion or wrap failure) | pooled (v1compat: audited-and-ran, NOT refused — see notes), session, wire ext | authority (enforcement) | **v1compat asymmetry, decided**: on the pooled path a non-transactional target under v1compat runs under classifier enforcement only, with an `readonly_unenforced` audit line (unitpolicy.go:169) — refusing would take the reader role away from every SQLite-class target for a guarantee never offered there. The session-profile arm refuses (fail closed) because the front door promises server-enforced read-only.  Justification: divergence:§6.2. |

## 3. Session-state gates (SET / LOCK / RESET — pooled allowlist vs wire denylist)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrSetGUCRefused` | decl session_state.go:33; session_state.go:201 (engine GUC), :205 (grammar GUC), :216 (off-allowlist SET) | pooled + session (allowlist model) | session-state | Pooled-path arm of the GUC model.  Justification: divergence:§6.4. |
| `ErrSetNotLocal` | decl session_state.go:30; session_state.go:212 | pooled + session | session-state | SET without LOCAL would leak to the next pool user.  Justification: divergence:§6.4. |
| `ErrSetOutsideTx` | decl session_state.go:39; session_state.go:220 (allowlist SET LOCAL outside tx), :406 (wire denylist SET LOCAL outside tx) | pooled, session, wire | session-state | The wire arm keeps the same identity for the analogous denylist rule.  Justification: divergence:§6.4 — arms on both GUC models; the extended frames do not dispatch it (§7.6). |
| `ErrWireSetRefused` | decl session_state.go:363; session_state.go:372/376/382 (wire denylist: parsing GUC, authority-in-disguise SET forms, reader search_path) | wire simple + wire ext (via `admitSessionState`) | session-state | Wire-only: the backend is discarded at close, so the pooled leak hazard does not exist and the denylist replaces the allowlist. **Two GUC models stay distinct — flattening them is a phase-1 mutation cell.**  Justification: divergence:§6.4 — the wire denylist arm. |
| `ErrLockOutsideTx` | decl session_state.go:44; session_state.go:233 | pooled, session, wire | session-state | A lock outside a transaction releases immediately — admitting it would be a lie.  Justification: applicability:§7.6 — reachable pooled/session/wire via admitSessionState; extended frames do not dispatch it. |
| `ErrGrammarDrifted` | decl dialect.go:150; dialect.go:181 (parsing mode readable but wrong at pool checkout) | pooled (checkout), wire (reported-parameter re-check, A26) | session-state | Currently raised at acquisition; the phase-1 A26 fold extends the re-read to per-statement on a pinned session.  Justification: divergence:§6.6 — raised at pool checkout today; the per-statement pinned re-read lands with the A26 fold.. |

## 4. Authority and transaction state (session machinery)

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `auth.ErrDenied` | the read floor (engine.go:471, session_engine.go:46), class floors (`authorizeUnit` wire_execute.go:231), control floors (wire_query.go:213-217, wire_execute.go:185-194) | all four | authority | The uniform denial — never discloses existence. Mapped to `CodeDenied`/SQLSTATE 42501-family per surface renderer. |
| `ErrTxAborted` | decl session_tx.go:43; wire_execute.go:160/204, session_engine.go:175, wire_query.go:226/247 | session, wire simple, wire ext | authority | Failed-transaction state; recovery controls only.  Justification: applicability:§7.5. |
| `ErrTxAlreadyOpen` | decl session_tx.go:34; session_tx.go:129 | session, wire (both via `handleTxControl`) | authority | BEGIN on an open transaction.  Justification: applicability:§7.5. |
| `ErrNoOpenTx` | decl session_tx.go:37; session_tx.go:288 | session, wire | authority | COMMIT/ROLLBACK with nothing to finish.  Justification: applicability:§7.5. |
| `ErrNoSuchTx` | decl txstatus.go:36; txstatus.go:83/:88 | session, wire, RPC status | authority | A transaction id with no progression — "not the caller's" answers exactly as "never existed", so tx.status cannot be used to discover which transaction ids exist.  Justification: applicability:§7.4. |
| `ErrTxChainUnsupported` | decl session_tx.go:55; session_tx.go:105 | session, wire | authority | e.g. ROLLBACK followed by BEGIN in one buffer shape; both boundaries must be recorded.  Justification: applicability:§7.5. |
| `ErrTxControlUnsupported` | decl txcontrol.go:71; txcontrol.go:265/360 | session, wire | authority | A transaction-control shape the machine does not own (WITH CONSISTENT SNAPSHOT and kin).  Justification: applicability:§7.5. |
| `ErrTxAuthorityChanged` | decl session_tx.go:50; wire_execute.go:89-92, session_engine.go:146-150 | session, wire | authority | Demotion mid-session; rollback cleanup failed or the transaction's authority ended.  Justification: applicability:§7.5. |
| `ErrSessionNotFound` | decl session.go:62; session.go:436, session_engine.go:131, wire_execute.go:79 | session, wire | authority | A close raced the call, or the id is not the caller's.  Justification: applicability:§7.5. |
| `ErrSessionBusy` | decl session.go:56; session.go:509, session_tx.go:568 | session, wire | authority | One statement at a time per session; a second concurrent call refuses.  Justification: applicability:§7.5. |
| `ErrConnectionDraining` | decl session.go:72; conns.go:65/110, session.go:329 | all four | authority | Connection deleted/closed mid-call. |
| `ErrSessionCapExceeded` / `ErrLeaseCapExceeded` / `ErrResidentBudgetExceeded` | decl session.go:67, session.go:297 and session.go:300; raised session.go:333, session.go:337, session.go:341, wire_session.go:345 and wire_session.go:347 | session/wire open | resource | Cap refusals at open; audited distinctly, uniform on the wire (front-door rows carry the identity).  Justification: applicability:§7.2； applicability:§7.2； applicability:§7.2. |
| `ErrWorkspaceNotFound` | decl workspaces.go:23; workspaces.go:80/104/129 | RPC admin surface only | authority | Not a gate refusal — listed for walk completeness (it is a public sentinel).  Justification: applicability:§7.3. |

## 5. Extended-protocol objects and transport

| sentinel | raised at | surfaces | layer | notes |
|---|---|---|---|---|
| `ErrExtendedUnsupportedTarget` | decl wire_extended.go:137; wire_extended.go:80 | wire ext | capability | Extended frames on a non-PostgreSQL target: refused loudly, never approximated.  Justification: applicability:§7.1. |
| `ErrDuplicateStatement` / `ErrDuplicatePortal` | decl wire_extended_objects.go:26 and wire_extended_objects.go:63; raised wire_extended_objects.go:465 and wire_extended_objects.go:522 | wire ext | resource | Bind/Parse naming a live named object.  Justification: applicability:§7.1； applicability:§7.1. |
| `ErrUnknownStatement` / `ErrUnknownPortal` | decl wire_extended_objects.go:49 and wire_extended_objects.go:67; raised wire_extended_objects.go:508 and wire_extended_objects.go:547 | wire ext | resource | Phantom name — the store keeps the record when in doubt (the 42P05 lesson).  Justification: applicability:§7.1； applicability:§7.1. |
| `ErrNamedObjectCap` | decl wire_extended_objects.go:39; wire_extended_objects.go:482/526 | wire ext | resource | 256 statements / 64 portals per session.  Justification: applicability:§7.1. |
| `ErrParamCap` | decl wire_extended_objects.go:45; wire_extended.go:322 | wire ext | intake | Bind > 8192 params.  Justification: applicability:§7.1. |
| `ErrPendingCloseCap` | decl wire_extended_objects.go:59; wire_extended.go:420 | wire ext | resource | A Close whose recovery obligation cannot be tracked — capacity refuses rather than discarding the obligation.  Justification: applicability:§7.1. |
| `ErrRetainedBudget` | decl wire_extended_objects.go:35; wire_extended_objects.go:694 | wire ext | resource | Retained-state reservation failure; nothing forwarded.  Justification: applicability:§7.1. |
| `ErrWireEmitNil` | decl wire_query.go:86; wire_extended.go:490/591/719 | wire ext (engine API) | transport | Programming-error guard on the emit callback.  Justification: applicability:§7.1. |
| `ErrWireFaceLost` | decl wire_query.go:757; wire_query.go:772 (wrap) | wire simple + ext | transport | The pinned connection's raw face is gone; session closes.  Justification: applicability:§7.1. |
| `ErrWireSequenceRefused` | decl wire_query.go:769; wire_query.go:519 | wire simple | transport | A message the loop does not support while an extended segment is open — end it with Sync first.  Justification: applicability:§7.1. |
| `ErrNotExecuted` | decl wire_query.go:186; wire_query.go:347, wire_extended.go:1182 | wire simple + ext | transport | The recorded OUTCOME of a statement PostgreSQL did not run because an earlier one failed — a history status, not a client refusal.  Justification: applicability:§7.1. |
| `ErrDecodedResultTruncated` | decl wire_query.go:745; wire_query.go:730 | wire simple (non-PostgreSQL decoded producer) | transport | A page-bound result on a target that cannot stream unbounded.  Justification: applicability:§7.1. |
| `ErrCancelKeyCollision` | decl cancel_registry.go:138; cancel_registry.go:168 | wire (engine internal) | transport | Cancel-key registry collision — CSPRNG pair uniqueness.  Justification: applicability:§7.1. |

## 6. Policy and semantic divergences, on record

A DIVERGENCE is a cross-surface difference in the answer a client can
observe, caused by a decision rather than by what physically exists. Each
one below is a ruling with its reason; the walk requires every
restricted-surface row to declare its justification category and to name
one of these entries.

1. **`ErrEmptyStatement`** — raised on token paths; tolerated on the wire (EmptyQueryResponse; empty Parse legal and charged). Decision: guard empty scripts UI-side. (Row §1.)
2. **`ErrReadOnlyUnenforceable`** — v1compat pooled runs unenforced with an audit line; session-profile pooled refuses; wire ext execute-scoped arm refuses. Decision: the promise differs by profile and surface because the guarantee offered differs. (Row §2.)
3. **`ErrStatementUnsupported` vs `ErrReaderAdvancedPattern` ordering** — the wire paths currently run reader analysis BEFORE the profile gate; pooled runs the reverse. **Decided (pipeline design matrix §7.4): profile admissibility precedes reader analysis everywhere**, because removing the UDF cannot make a v1compat data-modifying CTE runnable. This is the ONE intentional identity change of phase 1; its corpus rows are enumerated as a prediction before the flip, and per-row deltas go to a ruling, never to quiet regeneration.
4. **GUC models** — pooled allowlist (`ErrSetGUCRefused`/`ErrSetNotLocal`) vs wire denylist (`ErrWireSetRefused`; `ErrSetOutsideTx` has an arm on both models). Decision: the leak hazard genuinely differs (backend discarded at close); the phase-1 chain keeps them as two applicability sets of one stage. (Rows §3.)
5. **`onSession` control verbs** — `BEGIN`/`COMMIT`/`ROLLBACK`/`SET`/`RESET`/`LOCK` are admitted on the session profile ONLY on a session (engine action, never forwarded text); off one they refuse with `ErrStatementUnsupported`. The pooled `run` passes `pinned != nil`; every other admit site passes literal `true` because the surface implies it. (Row §2.)
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
3. **`ErrWorkspaceNotFound`** — an RPC admin-surface lookup identity, not a gate refusal; listed for walk completeness. (Row §4.)
4. **`ErrNoSuchTx`** — reachable from the RPC status verbs and the wire's transaction surfaces; a stateless pooled call has no transaction id to ask about. (Row §4.)
5. **Transaction-state machinery (§4 rows)** — `ErrTxAborted`, `ErrTxAlreadyOpen`, `ErrNoOpenTx`, `ErrTxChainUnsupported`, `ErrTxControlUnsupported`, `ErrTxAuthorityChanged`, `ErrSessionNotFound`, `ErrSessionBusy` and `ErrConnectionDraining` require a session-bound or wire-bound call to be reachable: the transaction machine and the session claim exist only there, and a stateless pooled `Execute` has no transaction state to refuse against (a pooled BEGIN refuses at the capability layer instead). (Rows §4.)
6. **Session-state gates need a session-shaped call** — `ErrSetOutsideTx` and `ErrLockOutsideTx` are reachable on pooled, session and wire (the SET/LOCK gates dispatch through `admitSessionState` on all three) but not on the extended protocol's own frames, where SET and LOCK arrive as Parse-gated statements whose session-state applicability the chain declares rather than the gate dispatching. (Rows §1, §3.)

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

## 9. Corpus prediction for the phase-1 ordering flip (recorded BEFORE any
    drive changes; see divergence §6.3)

The one intentional behaviour change of phase 1 moves the wire paths' reader
analysis to AFTER the profile gate. If any corpus statement sits in the
affected class — a read-only v1compat unit whose data-modifying CTE also
calls a user-defined function — its client-visible refusal identity changes
from the reader stage's to the profile's.

**The prediction: the manifest delta is EMPTY, and it is empty by
construction, not by luck.** The corpus replay's gate decision
(`gateDecision` in the corpus test: profile admit, then the WHERE guard)
never runs the reader stage at all, so no ordering change between admit and
reader analysis can reach the manifest. The one `refused:nested-mutation`
row in the committed manifest (000005_update_backfill_contacts.sql,
ordinal 3, verb UPDATE) refuses at the profile gate in BOTH orders and keeps
its identity.

**Therefore, at the Step that flips the wire paths:** a changed corpus
manifest has no third story available — it is a DEFECT, enumerated per row
(old identity, new identity) and surfaced for a ruling; it is not a
regeneration and not a carve-out. The structural reason is asserted by a
cell (`TestGateMatrix_CorpusReplayCannotSeeTheReaderStage`), which fails if
the replay ever grows a reader-analysis arm, because at that point the
prediction above goes stale and must be re-derived and re-recorded BEFORE
the corpus runs again.