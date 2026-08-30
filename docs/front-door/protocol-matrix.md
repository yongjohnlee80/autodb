# Front-door protocol matrix — PostgreSQL wire v3

**Status:** rev 1 — pre-F0 gate document (ADR-0075 §5). F0 implementation
does not begin until this matrix is lector-reviewed and accepted.
**Owner:** the F0 implementer (authored by jarvis as ADR-0075's author;
ownership transfers with F0). **Companion to:** KB ADR-0075 (accepted
2026-08-30); state semantics from KB ADR-0074 (ExecSession). Code-adjacent
per ADR-0074 §8: any implementation change that moves a cell edits this
file in the same PR.

This document pins, per protocol message and per connection state: whether
the front door supports, maps, or refuses it; the exact gating and audit
events; the resource charge and release points; and the error identity on
refusal. **No silent acceptance**: a message or parameter not named here is
a defect in this document, not an implementation freedom.

---

## 1. Conventions

### 1.1 Connection states

| State | Meaning |
|---|---|
| `S0 raw` | TCP accepted, nothing read. Global IP allowlist already passed at accept. |
| `S1 tls` | TLS established (`verify-full` contract server-side; client cert not required v1). |
| `S2 startup` | `StartupMessage` received, not yet authenticated. |
| `S3 auth` | Password exchange in progress (PAT over TLS). |
| `S4 ready-I` | Authenticated; ExecSession open; idle, no transaction (`ReadyForQuery('I')`). |
| `S4 ready-T` | In explicit transaction (`ReadyForQuery('T')`). |
| `S4 ready-E` | Failed transaction (`ReadyForQuery('E')`) — recovery controls only (gate matrix). |
| `S5 seg` | Inside an extended-query segment (between first `Parse`/`Bind` and `Sync`). Sub-state of any `S4`. |
| `S6 closing` | Terminate received, fatal error emitted, or lease/deadline expired; draining and releasing. |

Startup-phase (`S0`–`S3`) denials are **uniform** (ADR-0075 MF5): the same
error shape and timing for unknown user, bad token, expired token,
IP-not-allowed (user layer), and cap-exceeded — no enumeration oracle.
Detailed gate errors (§8a identities) exist **only from `S4` onward** (SF2).

### 1.2 Error shape

Gate refusals use the ADR-0074 §8a identity: accurate SQLSTATE, the gate
rule in `DETAIL` (stable identity string), remediation in `HINT`. Target
errors pass through with raw `*pgconn.PgError` fields **verbatim**. Errors
synthesized by the front door never impersonate the target: `S` (severity),
`C` (code) are accurate, and `DETAIL` carries the `frontdoor/...` or
`gate/...` rule id.

### 1.3 Audit vocabulary (front-door events)

`fd.conn_open`, `fd.tls_ok`, `fd.auth_ok`, `fd.auth_denied` (uniform;
internal reason recorded, wire reason generic), `fd.session_open` /
`fd.session_close` (with ExecSession id), `fd.stmt_attempt` (before every
effect — extended: at every `Execute`), `fd.stmt_outcome`, `fd.refused`
(gate rule id), `fd.cancel_received` / `fd.cancel_applied` /
`fd.cancel_stale`, `fd.backpressure_enter` / `fd.backpressure_exit`,
`fd.budget_refuse`, `fd.conn_close` (cause). Attempt-before-effect is
inherited from core: **no target-visible effect without a prior durable
attempt record.**

### 1.4 Memory budget lanes (lector matrix-acceptance criterion 1)

Two lanes against the global resident-memory budget (§4 of ADR-0075;
default 1 GiB):

- **General lane** — all segment input, retained statement/portal state,
  pending serialized output. Charged **before** read/decode/store/serialize.
- **Control/error lane** — a **reserved slice, default 8 MiB** (config
  `frontdoor.control_lane_bytes`, ceiling 32 MiB), admitting ONLY:
  `Close`, `Sync`, `Flush`, `Terminate` intake; `ErrorResponse` /
  `NoticeResponse` / `ReadyForQuery` emission; cancel processing; and
  teardown bookkeeping. Charges in this lane are bounded per connection
  (≤ 64 KiB) and never blocked by general-lane saturation — **a saturated
  budget can always still process the messages that release it.**
  The lane is sized so that all 320 max connections can simultaneously
  hold a control charge (320 × 64 KiB = 20 MiB > 8 MiB default — the
  per-connection control charge is therefore additionally capped by a
  per-connection reservation made at accept time, released at close;
  accept fails closed if the reservation cannot be made).

### 1.5 Weighting rule (criterion 2)

A charge is `max(wire_bytes_declared, decoded_estimate)` where
`decoded_estimate` weights: parameter count × per-param overhead, portal
result-buffer high-water, driver-facing buffers handed to the pinned
target connection, and re-framing buffers on the output path. Wire bytes
alone never under-charge a small message that decodes large (amplification:
e.g. a `Bind` with 8192 zero-length parameters charges its decoded array
overhead, not its ~16 KiB wire size).

---

## 2. Startup & authentication sequence

| # | Client sends | State | Decision | Charge | Audit | Error path |
|---|---|---|---|---|---|---|
| 2.1 | `SSLRequest` | S0 | **Required.** Answer `S`, begin TLS. A second `SSLRequest` after TLS = protocol violation → close. | pre-auth cap 64 KiB | `fd.conn_open` | plaintext `StartupMessage` at S0 → uniform denial, close (no fallback) |
| 2.2 | `GSSENCRequest` | S0 | **Refused**: answer `N`; if the client then proceeds without TLS → uniform denial + close. | pre-auth cap | — | — |
| 2.3 | `CancelRequest` | S0 | Accepted **without TLS** (protocol-conformant: cancel connections are plaintext); processed per §6; connection closed immediately after. | control lane | `fd.cancel_received` | stale/unknown key = silent no-op (`fd.cancel_stale`), close |
| 2.4 | `StartupMessage` (protocol 3.0) | S1 | Parse parameters per §3. Any refused parameter → uniform denial. | pre-auth cap | — | uniform denial (28000), close |
| 2.5 | `StartupMessage` (protocol ≠ 3.0, incl. 3.x minor > 0) | S1 | **Refused** via `ErrorResponse` (no `NegotiateProtocolVersion` in v1 — we implement exactly 3.0). | pre-auth cap | `fd.refused` | uniform denial, close |
| 2.6 | (server →) `AuthenticationCleartextPassword` | S2 | The only offered method. PATs are verified server-side against SHA-256 records; cleartext-over-TLS is the Q4-ratified design. SCRAM is **not offered** (hashed PATs cannot back SCRAM verifiers). | — | — | — |
| 2.7 | `PasswordMessage` (PAT) | S3 | Verify: token exists ∧ not expired ∧ not revoked ∧ user enabled ∧ **user-layer IP allowlist** ∧ per-user session cap ∧ global cap ∧ `frontdoor_max_leases`. All-or-nothing; which check failed is audit-only. | pre-auth cap | `fd.auth_ok` / `fd.auth_denied` | uniform denial (28000 `invalid_authorization_specification`), close. 3 attempts/conn, 10/min/source-IP. |
| 2.8 | `SASLInitialResponse` / `SASLResponse` / `GSSResponse` | S3 | **Protocol violation** (method not offered) → uniform denial, close. | control lane | `fd.auth_denied` | — |
| 2.9 | (server →) on success | S3→S4 | `AuthenticationOk`; `ParameterStatus` set per §3.3; `BackendKeyData` (CSPRNG, MF7); `ReadyForQuery('I')`. ExecSession opened; **session-lifetime lease acquired** — lease unavailable = uniform denial pre-`AuthenticationOk`. | session overhead charged | `fd.session_open` | — |

Deadlines: TLS/startup/auth 10s each (ceiling 60s). Pre-auth global
connection cap 64, auth workers 16.

---

## 3. Startup pinning

### 3.1 Accepted `StartupMessage` parameters

| Parameter | Decision |
|---|---|
| `user` | **Required.** Must equal the PAT's owner at auth time; mismatch = uniform denial (identity comes from the token; `user` is a cross-check, never an override). |
| `database` | **Required.** Must name an autodb **connection** the user holds a grant on (name or `conn:<id>`). Unknown/ungranted = uniform denial (no existence disclosure). |
| `application_name` | **Accepted + audited** (recorded on session + every audit row; also reported back via `ParameterStatus`). Length-capped 256 bytes (over = truncate + notice, audited verbatim). |
| `client_encoding` | Accepted iff `UTF8` (case-insensitive). Anything else → refused (uniform denial): autodb does not transcode. |
| `options` | **Refused if it sets any GUC** (`-c key=val` or `--key=val` content). ADR-0075 §5 pins this: GUC-setting options refuse with the §8a shape post-auth-impossible — at startup this is the uniform denial. An empty/whitespace `options` is accepted and ignored (audited). |
| `replication` | **Refused** (any value: `true`/`database`/`on`). |
| `_pq_.*` (protocol extensions) | **Refused** (uniform denial). We negotiate no extensions in v1; silently dropping them would be silent acceptance. |
| any other parameter | **Refused** (uniform denial). PostgreSQL treats unknown startup parameters as GUC attempts; we refuse rather than emulate GUC semantics. |

### 3.2 GUC policy (summary)

No client-controlled GUC reaches the pinned target connection at startup.
Post-auth, `SET` statements follow the ADR-0074 gate matrix (benign-SET
default admin-only; grammar GUCs banned forever; engine-originated
`SET LOCAL` belts are not client-reachable).

### 3.3 `ParameterStatus` set reported at session open

Forwarded **verbatim from the pinned target connection** at lease
acquisition (fidelity — the values are the real server's):
`server_version`, `server_encoding`, `client_encoding`, `DateStyle`,
`IntervalStyle`, `TimeZone`, `integer_datetimes`,
`standard_conforming_strings`. Synthesized by the front door:
`application_name` (echo of §3.1), `is_superuser` (**always `off`**),
`session_authorization` (the autodb username). Later `ParameterStatus`
messages from the target (e.g. a statement changed `DateStyle` — only
possible via an admitted `SET`) are **forwarded verbatim**. No other
parameters are reported in v1.

---

## 4. Frontend message matrix (post-auth, `S4`/`S5`)

Charge column: lane + when. "hdr-first" = the 4-byte declared length is
validated against `SetMaxBodyLen` (64 MiB) and charged **before** the body
is read (criterion 3); oversized declared length refuses before any read.

| Message | States | Decision | Gating | Charge | Audit | Refusal / notes |
|---|---|---|---|---|---|---|
| `Query` (simple) | S4-I/T/E | **Mapped: PostgreSQL implicit-transaction semantics** (MF2). Statements split and run in order; each individually classified/authorized/guarded; first error (gate or target) aborts the block, rolls back its earlier statements. Explicit `BEGIN`/`COMMIT` inside the buffer per PostgreSQL implicit-block rules (ExecSession state transitions, never passthrough). In S4-E: recovery controls only. | classify+authorize+guard per statement; attempt-before-effect per statement | general, hdr-first; output via pending-output watermark | `fd.stmt_attempt`/`fd.stmt_outcome` per statement | Empty query → `EmptyQueryResponse` + `ReadyForQuery` (control lane). |
| `Parse` | S4-I/T (opens S5) | **Native pinned-conn** (MF3). Gated at Parse: classifier + profile + grants; **immutable classification/guard metadata attached** to the statement. Named statements per lifetime rules; unnamed statement replaced per protocol. | classify+authorize at Parse | general, hdr-first; on `ParseComplete`, segment charge **transfers to retained** (statement text + param metadata, 16 MiB/session cap) | `fd.stmt_attempt` (parse-time gate) | Refused classes (COPY/LISTEN/cursor/PREPARE verbs) refuse **at Parse** with §8a; segment then discards through `Sync`. |
| `Bind` | S5 | Native: raw parameter formats/values and per-column result formats preserved bit-for-bit to the pinned conn. ≤ 8192 params. | none (authority is at Parse + Execute) | general, hdr-first + decoded weighting (param array); portal buffers charge retained on `BindComplete` | — | Param-count/size over limits → §8a refuse, discard-through-Sync. |
| `Describe` (`S`/`P`) | S5 | Native passthrough: `ParameterDescription` + `RowDescription`/`NoData` from the pinned conn (Describe-before-Execute metadata preserved). | none | control-sized (general lane) | — | Unknown name → target's error verbatim. |
| `Execute` | S5 | Native, **with Execute-time authority** (MF1): authority re-resolved + re-authorized at **every** Execute (portal re-executions included); fresh `fd.stmt_attempt` precedes every effect. Portal `maxRows` honored; `PortalSuspended` preserved; suspended portal buffers charge retained state. | re-authorize per Execute | general; output watermark; suspended buffers → retained | `fd.stmt_attempt`/`fd.stmt_outcome` per Execute | Grant revoked between Parse and Execute → §8a refuse at Execute (tested per ADR). |
| `Close` (`S`/`P`) | S4/S5 | Native; **releases** the named statement's/portal's retained charge. | none | **control lane** | — | Always admissible, even at budget saturation (criterion 1). |
| `Flush` | S5 | Native passthrough to output pump. | none | **control lane** | — | — |
| `Sync` | S4/S5 | Native: closes the segment, resets segment counters (10 000 msgs / 96 MiB), emits `ReadyForQuery` from the ExecSession state machine, ends post-error discard. | none | **control lane** | — | Always admissible (criterion 1). |
| `Terminate` | any S4/S5 | Clean close: open tx → ROLLBACK via ExecSession (audited); session closed; lease + all charges released. | — | **control lane** | `fd.conn_close(cause=terminate)` | Always admissible. |
| `CopyData`/`CopyDone`/`CopyFail` | any | **Protocol violation** (08P01): COPY is refused at classification, so no COPY sub-protocol is ever active; receiving these = fatal error + close. | — | control lane | `fd.refused` | COPY the *statement* refuses at Parse/Query gate with 0A000 (§7). |
| `FunctionCall` (legacy fast-path) | any | **Refused** (0A000, §8a `frontdoor/no-fastpath`): legacy surface bypasses text classification by construction. | — | control lane | `fd.refused` | Connection stays usable (refusal, not violation). |
| Unknown message type byte | any | Fatal protocol violation (08P01) → `ErrorResponse` + close. | — | control lane | `fd.refused` | Never skipped-and-continued. |

**Post-error segment discard:** after any error inside a segment, all
frontend messages except `Sync`/`Close`/`Terminate` are discarded (charged
to the control lane at header-size only, body skipped with bounded reads)
until `Sync` — pg-conformant discard-through-Sync.

---

## 5. Backend emission matrix

| Message | Source | Notes |
|---|---|---|
| `RowDescription`, `DataRow`, `CommandComplete`, `EmptyQueryResponse`, `ParameterDescription`, `ParseComplete`, `BindComplete`, `CloseComplete`, `NoData`, `PortalSuspended`, `NoticeResponse` (target), `ParameterStatus` (target-driven) | **Forwarded verbatim** from the pinned target conn (raw descriptors + raw cell bytes via `RawRows`, re-framed; §2a default-raw processor). | The wire never silently truncates: no row caps on the front door path; load control = engine `statement_timeout` + caps; a configured row cap refuses loudly (§8a) instead of shortening results. |
| `ErrorResponse` (target) | Forwarded with raw `*pgconn.PgError` fields verbatim. | Never rewritten. |
| `ErrorResponse` (gate/front door) | Synthesized per §1.2 (§8a identity). | Distinguishable via `DETAIL` rule id. |
| `ReadyForQuery` | **Synthesized** from the ExecSession state machine (§6). Never emitted on `ErrTxOutcomeUnknown` (§6.3). | |
| `AuthenticationCleartextPassword`, `AuthenticationOk`, `BackendKeyData`, `ParameterStatus` (session-open set §3.3), `NegotiateProtocolVersion` (never in v1) | Synthesized at startup. | `BackendKeyData` keys CSPRNG (MF7). |
| `CopyInResponse`/`CopyOutResponse`/`CopyBothResponse` | **Never emitted** — COPY refused pre-target. | Also asserted by conformance tests: seeing one from the target = defect (classifier bypass), fatal + audited. |

---

## 6. Transaction status & cancel semantics

### 6.1 `ReadyForQuery` byte ← ExecSession state

| ExecSession state | Byte |
|---|---|
| idle, no open tx | `I` |
| open tx, healthy | `T` |
| failed tx (25P02 semantics; recovery controls only) | `E` |

### 6.2 State transitions (pinned)

- Target error or **cancel inside a tx** → `T`→`E`.
- **Gate-only refusal** (nothing reached the target) → state unchanged
  (`T` stays `T`, `I` stays `I`).
- Clean COMMIT/ROLLBACK finalization → `I`.

### 6.3 Outcome-unknown

`ErrTxOutcomeUnknown` (golib dao-0017): audit `unknown_pending`, **discard
the target connection, close the frontend connection WITHOUT emitting
`ReadyForQuery`** — a false ready byte would assert a state nobody can
prove. Reconciliation is R4's outcome machine; the front door never
resolves it inline.

### 6.4 Cancel

`BackendKeyData` keys: CSPRNG, registered at session open, invalidated at
session close; stale keys are audited no-ops. Cancel is **statement-only**
and is **atomically detached before COMMIT/ROLLBACK finalization begins** —
a cancel can never race the 0017 finalizers. Cancel of an idle session is a
no-op. Race tests pinned by ADR: query/finalizer/disconnect/stale-key
boundaries.

---

## 7. Refusal catalogue (v1)

All with §8a shape; connection remains usable unless marked fatal.

| Surface | SQLSTATE | Rule id (`DETAIL`) | `HINT` |
|---|---|---|---|
| `COPY` (any form, either direction) | `0A000` | `gate/no-copy-v1` | Use the RPC/TUI surfaces or batch via INSERT; COPY is a candidate for a later phase. |
| `LISTEN` / `NOTIFY` / `UNLISTEN` | `0A000` | `gate/no-async-notify` | Not supported through the front door. |
| `DECLARE`/`FETCH`/`MOVE` cursors | `0A000` | `gate/no-sql-cursors` | Use extended-protocol portals (`maxRows`) instead. |
| SQL-text `PREPARE`/`EXECUTE`/`DEALLOCATE` | `0A000` | `gate/no-sql-prepare` | Use wire-level prepared statements (extended protocol). |
| `FunctionCall` message | `0A000` | `frontdoor/no-fastpath` | Use a SELECT. |
| GUC-setting startup `options` / unknown startup params / `replication` / `_pq_.*` | uniform startup denial (`28000`) | (audit-only pre-auth) | — |
| Non-3.0 protocol version | uniform startup denial | (audit-only) | — |
| `SET`-class statements post-auth | per ADR-0074 gate matrix (profile/role-gated; grammar GUCs always refused) | `gate/set-…` ids from 0074 §8a | — |
| Retained-state budget refusal | `53200` (`out_of_memory`) | `frontdoor/retained-budget` | Close unused prepared statements/portals, or raise the budget. |
| Row-cap configured on a front-door connection | `54000` | `frontdoor/no-silent-truncation` | Remove the cap or use the RPC surface; the wire never shortens results. |
| Out-of-state / unknown message | `08P01` (fatal) | `frontdoor/protocol-violation` | — |

Reader role: every statement wrapped read-only engine-side; write attempts
surface the target's `25006` verbatim (F3 proof paths).

---

## 8. Resource charging map (criteria 2 & 3)

### 8.1 Charge points (all charged BEFORE the resource exists)

| Path | Charged when | Lane | Released when |
|---|---|---|---|
| Message intake | header read: declared length validated (≤ 64 MiB) + charged before body read | general (control for §4-marked messages) | decode complete → either freed (transient) or transferred (below) |
| Extended segment accumulation | per message, counted against 10 000 msgs / 96 MiB | general | `Sync` (reset) or error-discard completion |
| Retained statements/portals | `ParseComplete`/`BindComplete`/`PortalSuspended`: **segment charge transfers to retained** (no double-charge, no gap) | general | `Close` of the object; session end; error discarding named objects per protocol rules |
| Pending serialized output | before serialization of each outbound frame; watermark 4 MiB pauses target reads (backpressure, audited) | general | bytes flushed to socket |
| Per-connection control reservation | at accept | control | connection close |
| Session overhead (lease, registries) | at session open | general (fixed size) | session close |

### 8.2 Release-on-every-path proof obligation

Every error/disconnect path releases all three charge classes; F0 must
prove by test: (a) refuse-at-Parse, (b) error-mid-segment + discard-to-Sync,
(c) client disconnect mid-segment, (d) budget-refusal path itself,
(e) `ErrTxOutcomeUnknown` teardown, (f) lease-expiry close, (g) cancel
during Execute, (h) TLS-layer abort. **Leak detector:** the budget's
accounted total must return to the per-connection fixed overhead after each
test, and to ~0 after connection close — asserted, not observed.

### 8.3 Overflow safety

Declared lengths are `int32`-validated before use (negative/`> max` refuse
without reading); all accounting in `int64`; charge arithmetic checked for
overflow (a sum that would exceed `MaxInt64/2` refuses — cheap guard far
above any legal configuration).

### 8.4 Worst-case statement

True worst-case resident memory = global budget (1 GiB default) +
per-connection fixed overhead (TLS + decoder ≈ 64 KiB × 320 connections
≈ 20 MiB) + control lane (8 MiB). Nothing allocates before charging;
therefore the budget bounds the aggregate regardless of per-object shape
limits (whose naive product is ~29 GiB — shape limits are NOT the bound).

---

## 9. Limits applicability (from ADR-0075 §4 defaults)

| Limit (default / ceiling) | Applies at |
|---|---|
| TLS/startup/auth deadlines 10s / 60s | §2 rows 2.1–2.7 |
| Partial-frame progress 30s / 300s | every message body read + write progress (§4, §5) |
| Between-messages: ExecSession state deadlines (30m idle; 90s / 10m debug idle-in-tx; max-tx) | S4 idle; refreshed on message/state transitions |
| `SetMaxBodyLen` 64 MiB / 256 MiB | every post-auth message (hdr-first) |
| Segment 10 000 msgs / 96 MiB (reset at Sync) | §4 `Parse`…`Sync` |
| Retained state 16 MiB / 64 MiB per session | `Parse`/`Bind`/`PortalSuspended` transfers |
| Pending output watermark 4 MiB / 16 MiB | §5 emissions (backpressure) |
| Cumulative output 8 GiB per statement (audited accounting cap) | `Execute`/`Query` result streaming |
| Bind params 8192 / 65535 | `Bind` |
| Named statements 256/1024, portals 64/256 per session | `Parse`/`Bind` named objects |
| Pre-auth message cap 64 KiB / 256 KiB | §2 rows 2.1–2.8 |
| Global budget 1 GiB / 4 GiB (+ control lane §1.4) | §8 |
| Pre-auth conns 64 / auth workers 16 | accept + §2 |
| Auth attempts 3/conn, 10/min/IP | §2 row 2.7 |
| `reserved_headroom` 4; `frontdoor_max_leases` derived ≥ 1 | lease acquisition (§2 row 2.9) |

---

## 10. Conformance hooks (F4 ladder anchors)

- Simple-protocol implicit-tx suite (MF2 semantics; `psql` interactive).
- lib/pq + sqlx conformance (LM's real client: mixed simple/extended,
  text formats, unnamed statements).
- pgx-class suite (binary formats, statement-cache prepare/eviction).
- Discard-through-Sync, named/unnamed lifetimes, `PortalSuspended`.
- Grant-revoked-between-Parse-and-Execute refusal.
- Cancel races (query/finalizer/disconnect/stale-key).
- §8.2 release-on-every-path leak assertions; frame/length fuzzing
  (header-first property: no read past a refused header).
- Uniform-denial timing test (startup denials indistinguishable across
  causes — measured, not asserted).
- CopyInResponse-from-target canary (§5): classifier bypass detector.

## 11. Open items for lector

1. §1.4 control-lane sizing: fixed 8 MiB slice + per-connection 64 KiB
   reservation at accept — is fail-closed-at-accept the right posture, or
   should accept degrade (shed oldest idle pre-auth conn) first?
2. §3.1 `client_encoding`: refuse-non-UTF8 vs accept-and-forward (target
   would transcode; we chose refuse to keep byte-fidelity claims honest).
3. §2.5: no `NegotiateProtocolVersion` in v1 (hard-refuse non-3.0) —
   acceptable, or emit it for 3.x minors with empty extension list?
4. §7 SQLSTATE choices for budget (`53200`) and truncation-refusal
   (`54000`) — confirm identities against ADR-0074 §8a registry.
