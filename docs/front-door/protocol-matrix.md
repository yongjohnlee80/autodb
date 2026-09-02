# Front-door protocol matrix — PostgreSQL wire v3

**Status:** rev 5 — pre-F0 gate document (ADR-0075 §5). F0 implementation
does not begin until this matrix is lector-reviewed and accepted.
Rev 2 folds lector r0: MF1 full-check atomic auth/reservation; MF2
pg-conformant discard-through-Sync (Close NOT exempt) + segment entry on
any extended message + the complete object-release rules; MF3 composed
lane arithmetic, two-stage charging, reserve-before-target; MF4
NegotiateProtocolVersion for 3.x minors + the type-`p` frame reality;
MF5 direct-TLS/cert/error behavior + UTF8-pinned lease + full
ParameterStatus forwarding; MF6 limit identities in the refusal
catalogue. Lector's four r0 rulings are recorded in §11. Rev 3 folds the r1
remnants: version-negotiation consistency across §5/§7 (MF1), the
frontdoor_max_leases pointer to row 2.7's atomic reservation (MF2), the
complete never-emitted backend canary set (MF3), and certificate
fail-start before bind/listen + client-side verify-full phrasing (MF4);
the r1 direct-TLS ruling (v1 refusal accepted) closes §11's open item.
Rev 4 folds ADR-0075 **Amendment 1** into row 2.7: IP admission is
`(global ∨ user-layer) ∧ PAT-if-set`, not the AND rev 3 described. Edited by
the F0 implementer alongside the code that implements it, per the
code-adjacent rule.

**Rev 5 (F0e) moves four cells, all of them narrowings the implementation
forced.** Each is called out where it lands, and none was a free choice:

1. **Auth attempts per connection: 3 → 1.** Cleartext has ONE round, and
   re-prompting is not a defence — libpq answers a repeated
   `AuthenticationCleartextPassword` with the same password it already sent,
   so a ceiling of three spends three PAT verifications on one wrong
   password. That is amplification pointed at the server. PostgreSQL closes
   on the first failure and so do we; three remains the right ceiling the day
   a multi-round method is offered, and there is none to offer.
2. **Per-source-IP: 10 attempts/min → 10 FAILED attempts/min.** Counting
   successes would cap every connection pool in the estate at ten
   connections a minute from one host — an outage with a security story
   attached, firing on the most ordinary event in the system, an application
   restarting and refilling its pool. Failures charged: a denied credential,
   a pre-auth protocol violation, a refused startup, and (per row 2.1b) a
   TLS handshake failure. **Not** charged: a successful login, a failure of
   OUR meta store, and a peer that opened a connection and went away without
   asking for anything — the last because that is what a TCP health check
   looks like from here, and throttling an operator's own monitoring is a
   limiter configured by nobody, defending against nothing.
3. **Row 2.9's `ParameterStatus` set splits across F0e and F1.** §3.3
   requires the target's own reported set forwarded VERBATIM, and that set
   does not exist until a lease is held. F0e sends the three synthesized
   values only; the forwarded set arrives with the lease in F1. Sending a
   plausible fixed list in the meantime is what §3.3 exists to forbid.
4. **§8.5 is new: the allocation-to-charge map.** Two 64 KiB constants in two
   packages both had comments mentioning "TLS" and "decoder", so they read as
   one charge taken twice. They are different terms of §8.4's worst case, and
   the map names what each covers, which budget it is charged against, and
   which allocation is bounded by the connection cap rather than by a charge.
5. **Accept-time refusals are audited `fd.budget_refuse`** (§1.3's existing
   vocabulary) with the internal reasons `frontdoor/source-ip-throttled`,
   `frontdoor/connection-cap`, `frontdoor/pre-auth-connection-cap` and
   `frontdoor/control-lane-exhausted`. They close WITHOUT a frame: nothing
   has negotiated TLS at that point, so a PostgreSQL error would be
   unreadable bytes to a client waiting for an `S` or an `N`.

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
| `S1 tls` | TLS established. `verify-full` is the CLIENT's DSN contract (ADR-0075 MF4): the server's obligation is presenting certificate identity that verification succeeds against — hostname SAN, valid chain, in-validity. Client certificates are not required in v1. |
| `S2 startup` | `StartupMessage` received, not yet authenticated. |
| `S3 auth` | Password exchange in progress (PAT over TLS). |
| `S4 ready-I` | Authenticated; ExecSession open; idle, no transaction (`ReadyForQuery('I')`). |
| `S4 ready-T` | In explicit transaction (`ReadyForQuery('T')`). |
| `S4 ready-E` | Failed transaction (`ReadyForQuery('E')`) — recovery controls only (gate matrix). |
| `S5 seg` | Inside an extended-query segment: entered by **any** extended-protocol message (`Parse`, `Bind`, `Describe`, `Execute`, `Close`, `Flush`) — a segment legally starts with `Bind`/`Describe`/`Execute` against named objects surviving from earlier segments — and left at `Sync`. Sub-state of any `S4`. |
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
`fd.budget_refuse` (rev 5: also the accept-time refusals —
`frontdoor/source-ip-throttled`, `frontdoor/connection-cap`,
`frontdoor/pre-auth-connection-cap`, `frontdoor/control-lane-exhausted` —
which close without a frame, before any handler allocates for the
connection, so `fd.conn_open` is absent for them by construction),
`fd.conn_close` (cause). Attempt-before-effect is
inherited from core: **no target-visible effect without a prior durable
attempt record.**

### 1.4 Memory budget lanes (lector matrix-acceptance criterion 1)

Two lanes against the global resident-memory budget (§4 of ADR-0075;
default 1 GiB):

- **General lane** — all segment input, retained statement/portal state,
  pending serialized output. Charged **before** read/decode/store/serialize.

  **Composition (RULED, jarvis as lead, 2026-09-03).** The general lane admits
  three charges. ONE is a reservation — a statement's output working set
  (`pendingOutputWatermark`), taken before dispatch — and the lane's floor is
  derived from it: **global session cap × watermark**, so that at full occupancy
  every session can hold one output working set; config may only RAISE the lane,
  and startup refuses a lane below the floor. The other two are **caps on one session**, admitted by
  **backpressure** rather than reserved. Segment input (cap 96 MiB per segment)
  is charged as it occurs, header first (§1.5). Retained statement/portal state
  (cap 16 MiB per session) is **reserved BEFORE the `Parse`/`Bind` is forwarded to
  the target**, **finalized** at `ParseComplete`/`BindComplete`/`PortalSuspended`
  (the segment charge transfers to retained — no double-charge, no gap), and
  **released on a pre-Complete error** (:263, :264, :407, r0 MF3: the target must
  never hold a server-side prepared statement the budget did not admit). It is
  then released again when the object dies — closed, taken by its statement's
  cascade, ended with the transaction, or destroyed with the unnamed pair by a
  simple `Query` (§4a) — **owed: retained-state charging, F2b.** F2 owns those lifetimes and does not yet
  charge them, so that clause states the contract, not current behaviour, and §4
  and §4a stay `awaiting` until it lands. Backpressure: at full occupancy a new segment's
  intake waits for any statement to release its working set, which is the §7
  contract, bounded by the segment-stall budget (§8.4) and §8.2's release on
  every exit. They are deliberately NOT reserved: their sum over the session cap
  (≈29 GiB) exceeds the 4 GiB ceiling seven times, which is the proof they were
  never a composition. What the output reservation has that the others lack is a
  KNOWABLE bound per statement; that is why it composes and they do not.
- **Control/error lane** — a reserved slice admitting ONLY: `Sync`,
  `Flush`, `Terminate` intake (and header-only reads while discarding);
  `ErrorResponse` / `NoticeResponse` / `ReadyForQuery` emission; cancel
  processing; and teardown bookkeeping. Never blocked by general-lane
  saturation — **a saturated budget can always still process the messages
  that release it.** (`Close` releases retained state but is NOT
  control-lane-exempt from discard semantics — see §4; its intake charge
  is control-lane-sized.)

  **Composition rule (MF3, binding):** per-connection control reservation
  = **64 KiB**, made **atomically at accept** together with the
  connection slot; the lane default is **`max_frontend_connections ×
  64 KiB` = 20 MiB at the 320-connection default** (config
  `frontdoor.control_lane_bytes` may only raise it; validation fails
  startup if `lane < max_conns × 64 KiB`). Accept **fails closed** when
  the reservation cannot be made; existing connections are **never
  evicted** to admit a new one (lector r0 ruling 1). The lane is
  additive to the 1 GiB general budget.

### 1.5 Weighting rule (criterion 2) — two-stage charging (MF3)

The decoded size is unknowable before decode, so charging is two-stage:

1. **Stage 1 — declared-wire precharge:** the 4-byte declared length is
   validated and charged **before the body is read** (§8.3).
2. **Stage 2 — decoded delta:** before any decoded allocation is made,
   compute `decoded_estimate` (parameter count × per-param overhead,
   portal result-buffer high-water, driver-facing buffers handed to the
   pinned target connection, re-framing buffers on the output path) and
   charge **`decoded_estimate − wire_precharge` when positive, before
   allocating**. Failure of the delta charge refuses the message with
   the stage-1 charge released.

Wire bytes alone never under-charge a message that decodes large
(amplification: a `Bind` with 8192 zero-length parameters delta-charges
its decoded array overhead over its ~16 KiB wire size, before the array
exists).

---

## 2. Startup & authentication sequence

| # | Client sends | State | Decision | Charge | Audit | Error path |
|---|---|---|---|---|---|---|
| 2.1 | `SSLRequest` | S0 | **Required.** Answer `S`, begin TLS. A second `SSLRequest` after TLS = protocol violation → close. | pre-auth cap 64 KiB | `fd.conn_open` | plaintext `StartupMessage` at S0 → uniform denial, close (no fallback) |
| 2.1a | Direct TLS (PG 17 `sslnegotiation=direct`: first bytes are a TLS ClientHello, ALPN `postgresql`) | S0 | **Refused in v1**: bytes that are neither a length-prefixed request nor within the pre-auth cap → close immediately (audited `fd.tls_fail(direct-tls-unsupported)`). Candidate for a later rev; clients fall back to `SSLRequest` negotiation per libpq defaults. | pre-auth cap | `fd.tls_fail` | — |
| 2.1b | TLS layer behavior | S0→S1 | Server cert/key from config, **validated at startup BEFORE bind/listen** (ADR-0075: absent, unparsable, expired/not-yet-valid material, a broken chain, or a key/cert mismatch **fails start or enable** — the front door never listens with identity it cannot prove; SAN coverage of the configured host names is checked and a mismatch fails start too). **Reload on config-reload applies to NEW connections only** (established sessions keep their session keys — never dropped by a cert rotation); a reload with invalid material is REFUSED, keeping the last-good identity, loudly audited. TLS handshake failure: `fd.tls_fail(reason)` audited, close, **counted against the per-source-IP auth rate** (10/min) so handshake grinding is throttled with auth grinding. **Rev 5 exception:** a peer that opened the connection and sent NOTHING (`peer-gone-before-startup`) is audited and NOT counted — that shape is a TCP health check, and charging it would throttle an operator's own monitoring out of the estate within a minute. | — | `fd.tls_ok` / `fd.tls_fail` | — |
| 2.2 | `GSSENCRequest` | S0 | **Refused**: answer `N`; if the client then proceeds without TLS → uniform denial + close. | pre-auth cap | — | — |
| 2.3 | `CancelRequest` | S0 | Accepted **without TLS** (protocol-conformant: cancel connections are plaintext); processed per §6; connection closed immediately after. | control lane | `fd.cancel_received` | stale/unknown key = silent no-op (`fd.cancel_stale`), close |
| 2.4 | `StartupMessage` (protocol 3.0) | S1 | Parse parameters per §3. Any refused parameter → uniform denial. | pre-auth cap | — | uniform denial (28000), close |
| 2.5 | `StartupMessage` (major 3, minor > 0, and/or unrecognized `_pq_.*` options) | S1 | **Negotiate down (MF4, ruling 3)**: emit `NegotiateProtocolVersion` (newest supported = 3.0, plus the list of unrecognized `_pq_.*` option names) and **continue at 3.0 semantics** — pg-conformant, never a hard refusal. | pre-auth cap | — | — |
| 2.5a | `StartupMessage` (major ≠ 3) | S1 | **Refused**: uniform denial, close (unsupported major). | pre-auth cap | `fd.refused` | uniform denial, close |
| 2.6 | (server →) `AuthenticationCleartextPassword` | S2 | The only offered method. PATs are verified server-side against SHA-256 records; cleartext-over-TLS is the Q4-ratified design. SCRAM is **not offered** (hashed PATs cannot back SCRAM verifiers). | — | — | — |
| 2.7 | `PasswordMessage` (PAT) | S3 | **Full verification chain (MF1), then one atomic reservation.** Verify: token exists ∧ **is a front-door PAT** (scope check — no other credential class authenticates here) ∧ not expired ∧ not revoked ∧ owner matches startup `user` ∧ user enabled ∧ **IP admission: (global allowlist ∨ the user's `user_ip_allowlist` rows)** ∧ **PAT `allowed_ips` if set** (ADR-0075 **Amendment 1**, Johno 2026-08-31 — this was an AND of the two layers until the amendment; OR because the global list carries shared infrastructure and under AND a colleague at an already-listed office still needed a personal row, while a home address had to be listed GLOBALLY to be usable, bloating the perimeter and making the per-user layer a second registration rather than a narrowing. Accepted cost, on the record: a stolen PAT works from any globally-listed address for any account; per-token `allowed_ips` is the mitigation. Empty `allowed_ips` INHERITS the admission set — it does not mean "nowhere". The admission SOURCE, global or user-row, is audited) ∧ **target validation**: the `database` connection exists ∧ is enabled ∧ the user holds a grant on it ∧ its profile admits front-door use. Then **atomically reserve, as ONE operation**: per-user session slot (8) + global slot (256) + target lease (`frontdoor_max_leases`) + the session's fixed overhead charge — no check-then-reserve gap (a cap observed free must be the cap acquired); partial reservation is impossible, failure rolls back nothing-held. Which check failed is audit-only. | pre-auth cap | `fd.auth_ok` / `fd.auth_denied` | uniform denial (28000 `invalid_authorization_specification`), close. **1 attempt/conn** (rev 5: was 3 — cleartext has one round and re-prompting spends N verifications on one wrong password; 3 returns if a multi-round method is ever offered), **10 FAILED attempts/min/source-IP** (rev 5: was "attempts"; counting successes caps every pool at ten connections a minute from one host). Lease-cap failure: wire = the same uniform 28000; audit identity = `lease-cap-exceeded` (ruling 4). |
| 2.8 | Any other type-`p` frame in S3 | S3 | **There is no distinguishable SASL path (MF4):** once `AuthenticationCleartextPassword` is offered, every type-`p` frame IS a `PasswordMessage` by protocol — SASL-shaped bytes are simply a wrong password. Verified per row 2.7; fails the token lookup; uniform denial. `GSSResponse` is likewise type-`p` and takes the same path. | pre-auth cap | `fd.auth_denied` | uniform denial, close |
| 2.9 | (server →) on success | S3→S4 | `AuthenticationOk`; `ParameterStatus` set per §3.3; `BackendKeyData` (CSPRNG, MF7, **4-byte secret** — every client is negotiated down to 3.0 by row 2.5 and 3.0's cancel key is a fixed int32; pgproto3 models it as a `[]byte` for 3.2 and would happily send a longer one to a client that reads four and then loses every frame boundary after it); `ReadyForQuery('I')`. **Rev 5 split:** F0e emits the three SYNTHESIZED statuses only; §3.3's verbatim forwarded set needs the target's own reported values and therefore a lease, which is F1's. The ExecSession, lease, and all slots were already **atomically reserved in row 2.7** — nothing acquired between `AuthenticationOk` and ready. | (charged in 2.7) | `fd.session_open` | — |

Deadlines: TLS/startup/auth 10s each (ceiling 60s). Pre-auth global
connection cap 64, auth workers 16.

---

## 3. Startup pinning

### 3.1 Accepted `StartupMessage` parameters

| Parameter | Decision |
|---|---|
| `user` | **Required.** Must equal the PAT's owner at auth time; mismatch = uniform denial (identity comes from the token; `user` is a cross-check, never an override). |
| `database` | **Required.** Must name an autodb **connection** the user holds a grant on (name or `conn:<id>`). Unknown/ungranted = uniform denial (no existence disclosure). |
| `application_name` | **Accepted + audited** (recorded on session + every audit row; also reported back via `ParameterStatus`). Length-capped 256 bytes (over = truncate + notice, audited verbatim). **Current state (2026-09-03):** the echo and the cap/notice/verbatim-audit are implemented; recording on the session and every audit row is this row's CONTRACT and is `awaiting` the F1 wire loop (claim `#session-audit`). **Not forwarded to the target:** autodb sets no `application_name` on the backends it pins (the connection's DSN is used unchanged), so a fresh backend shows the target's own effective startup default — the DSN's value if supplied, otherwise the applicable server/database/role default (`ALTER DATABASE`/`ALTER ROLE … SET`), commonly empty — and two `psql` clients are indistinguishable there, and no backend PID is captured, so backend → session mapping does not exist yet on either side. **The client can change the backend's value at runtime:** `SET application_name` is refused by the session-state gate, but `SELECT set_config('application_name', …, false)` is classified as a read and runs on the pinned backend (lector, PR #51, proven live) — refusing `SET` makes no GUC immutable. *A structured per-session stamp at pin time (target caps at 63 bytes) is a documented gap, not a decision; it must refuse or survive the `set_config` overwrite.* |
| `client_encoding` | Accepted iff `UTF8` (case-insensitive). Anything else → refused (uniform denial): autodb does not transcode (ruling 2). **The target lease is pinned UTF8** — at lease acquisition the pinned connection's `client_encoding`/`server_encoding` `ParameterStatus` must report UTF8-compatible values, else lease acquisition fails loudly (audited); the byte-fidelity claim is only honest if both ends of the relay actually speak UTF8. |
| `options` | **Refused if it sets any GUC** (`-c key=val` or `--key=val` content). ADR-0075 §5 pins this: GUC-setting options refuse with the §8a shape post-auth-impossible — at startup this is the uniform denial. An empty/whitespace `options` is accepted and ignored (audited). |
| `replication` | **Refused** (any value: `true`/`database`/`on`). |
| `_pq_.*` (protocol extensions) | **Negotiated, not refused (MF4)**: unrecognized `_pq_.*` names are listed in `NegotiateProtocolVersion` (row 2.5) and the session continues at 3.0 with none of them active — the pg-conformant declination, and still no silent acceptance (the client is told exactly what was declined). |
| any other parameter | **Refused** (uniform denial). PostgreSQL treats unknown startup parameters as GUC attempts; we refuse rather than emulate GUC semantics. |

### 3.2 GUC policy (summary)

No client-controlled GUC reaches the pinned target connection at startup.
Post-auth, `SET` statements follow the ADR-0074 gate matrix (benign-SET
default admin-only; grammar GUCs banned forever; engine-originated
`SET LOCAL` belts are not client-reachable).

### 3.3 `ParameterStatus` set reported at session open

**Every `ParameterStatus` the pinned target connection presented at its
own connect is forwarded verbatim at session open (MF5)** — not a fixed
list: the server decides what is `GUC_REPORT`, and a fixed enumeration
would silently drop statuses added by newer servers (`search_path` on
PG 14+, `in_hot_standby`, `scram_iterations`, …). Three values are
**overridden with synthesized ones** after the forwarded set:
`application_name` (echo of §3.1), `is_superuser` (**always `off`**),
`session_authorization` (the autodb username — the CANONICAL account name,
not the client's spelling of it, which row 2.7 has already matched
case-insensitively: the identity a session reports should be the one the
grants are written against). **Rev 5:** the three synthesized values ship in
F0e; the forwarded set ships in F1, with the lease that produces it. Later `ParameterStatus`
messages from the target during the session are **forwarded verbatim**,
whatever their name — future-safe by construction. The UTF8 pin (§3.1)
is validated against this forwarded set at lease acquisition.

---

## 4. Frontend message matrix (post-auth, `S4`/`S5`)

Charge column: lane + when. "hdr-first" = the 4-byte declared length is
validated against `SetMaxBodyLen` (64 MiB) and charged **before** the body
is read (criterion 3); oversized declared length refuses before any read.

| Message | States | Decision | Gating | Charge | Audit | Refusal / notes |
|---|---|---|---|---|---|---|
| `Query` (simple) | S4-I/T/E | **Mapped: PostgreSQL implicit-transaction semantics** (MF2). Statements split and run in order; each individually classified/authorized/guarded; first error (gate or target) aborts the block, rolls back its earlier statements. Explicit `BEGIN`/`COMMIT` inside the buffer per PostgreSQL implicit-block rules (ExecSession state transitions, never passthrough). In S4-E: recovery controls only. | classify+authorize+guard per statement; attempt-before-effect per statement | general, hdr-first; output via pending-output watermark | `fd.stmt_attempt`/`fd.stmt_outcome` per statement | Empty query → `EmptyQueryResponse` + `ReadyForQuery` (control lane). |
| `Parse` | S4-I/T (opens S5) | **Native pinned-conn** (ADR MF3). Gated at Parse: classifier + profile + grants; **immutable classification/guard metadata attached** to the statement. Named statements per §4a lifetime rules; unnamed statement replaced per protocol. **Retained capacity for the statement is RESERVED against the 16 MiB session cap BEFORE the Parse is forwarded to the target** (reserve → forward → finalize on `ParseComplete`; released on error) — the target must never hold a server-side prepared statement the budget didn't admit (r0 MF3). | classify+authorize at Parse | general, hdr-first + stage-2 delta; retained **reserved pre-forward**, finalized at `ParseComplete` | `fd.stmt_attempt` (parse-time gate) | Refused classes (COPY/LISTEN/cursor/PREPARE verbs) refuse **at Parse** with §8a; segment then discards through `Sync`. Reservation failure → `53400` refuse, nothing forwarded. |
| `Bind` | S5 | Native: raw parameter formats/values and per-column result formats preserved bit-for-bit to the pinned conn. ≤ 8192 params. **Portal retained capacity reserved BEFORE forwarding**, finalized at `BindComplete`, released on error. | none (authority is at Parse + Execute) | general, hdr-first + stage-2 delta (param array pre-allocation); retained reserved pre-forward | — | Param-count/size over limits → §8a refuse, discard-through-Sync. |
| `Describe` (`S`/`P`) | S5 | Native passthrough: `ParameterDescription` + `RowDescription`/`NoData` from the pinned conn (Describe-before-Execute metadata preserved). | none | control-sized (general lane) | — | Unknown name → target's error verbatim. |
| `Execute` | S5 | Native, **with Execute-time authority** (MF1): authority re-resolved + re-authorized at **every** Execute (portal re-executions included); fresh `fd.stmt_attempt` precedes every effect. Portal `maxRows` honored; `PortalSuspended` preserved; suspended portal buffers charge retained state. | re-authorize per Execute | general; output watermark; suspended buffers → retained | `fd.stmt_attempt`/`fd.stmt_outcome` per Execute | Grant revoked between Parse and Execute → §8a refuse at Execute (tested per ADR). |
| `Close` (`S`/`P`) | S4/S5 (healthy) | Native; **releases** the named statement's/portal's retained charge; closing a prepared statement **cascades to its portals** (§4a). Intake is control-lane-sized so saturation cannot block release — but **during post-error discard `Close` is discarded like everything else** (MF2): PostgreSQL processes nothing but `Sync`/`Terminate` until the segment ends, and conformance wins; release then happens via `Sync` (segment discard) or the §4a rules. | none | **control lane** | — | — |
| `Flush` | S5 | Native passthrough to output pump. Discarded during post-error discard. | none | **control lane** | — | — |
| `Sync` | S4/S5 | Native: closes the segment, resets segment counters (10 000 msgs / 96 MiB), emits `ReadyForQuery` from the ExecSession state machine, ends post-error discard, releases the discarded segment's charges. | none | **control lane** | — | Always admissible, even at budget saturation (criterion 1). |
| `Terminate` | any S4/S5 | Clean close: open tx → ROLLBACK via ExecSession (audited); session closed; lease + all charges released. | — | **control lane** | `fd.conn_close(cause=terminate)` | Always admissible. |
| `CopyData`/`CopyDone`/`CopyFail` | any | **Protocol violation** (08P01): COPY is refused at classification, so no COPY sub-protocol is ever active; receiving these = fatal error + close. | — | control lane | `fd.refused` | COPY the *statement* refuses at Parse/Query gate with 0A000 (§7). |
| `FunctionCall` (legacy fast-path) | any | **Refused** (0A000, §8a `frontdoor/no-fastpath`): legacy surface bypasses text classification by construction. | — | control lane | `fd.refused` | Connection stays usable (refusal, not violation). |
| Unknown message type byte | any | Fatal protocol violation (08P01) → `ErrorResponse` + close. | — | control lane | `fd.refused` | Never skipped-and-continued. |

**Post-error segment discard (MF2 — pg-conformant):** after any error
inside a segment, **every** frontend message is discarded (charged to the
control lane at header-size only, body skipped with bounded reads) until
`Sync` — with exactly the two exemptions PostgreSQL's own
`ignore_till_sync` grants: **`Sync`** (ends the discard) and
**`Terminate`** (closes the connection). `Close`, `Flush`, `Parse`,
`Bind`, `Describe`, `Execute` are all discarded, exactly as the real
server would.

### 4a. Object-release rules (MF2 — the complete set)

Retained charges release when their object dies; the death rules are
PostgreSQL's own:

| Event | Releases |
|---|---|
| `Close S name` | the named prepared statement **and every portal constructed from it** (protocol-documented cascade). |
| `Close P name` | that portal. |
| `Parse` naming the unnamed statement (`""`) | the previous unnamed statement (implicit replacement). |
| `Bind` naming the unnamed portal (`""`) | the previous unnamed portal (implicit replacement). |
| Simple `Query` | the unnamed statement and unnamed portal (protocol-documented destruction). |
| **Transaction end** (COMMIT or ROLLBACK, incl. implicit-block end and failed-tx recovery) | **all portals** (named and unnamed) — portals do not survive the transaction. Prepared statements survive. |
| Error mid-segment | the erroring segment's in-flight (unfinalized) reservations; surviving named objects keep theirs. |
| Session end (any cause) | everything retained by the session. |

---

## 5. Backend emission matrix

| Message | Source | Notes |
|---|---|---|
| `RowDescription`, `DataRow`, `CommandComplete`, `EmptyQueryResponse`, `ParameterDescription`, `ParseComplete`, `BindComplete`, `CloseComplete`, `NoData`, `PortalSuspended`, `NoticeResponse` (target), `ParameterStatus` (target-driven) | **Forwarded verbatim** from the pinned target conn (raw descriptors + raw cell bytes via `RawRows`, re-framed; §2a default-raw processor). | The wire never silently truncates: no row caps on the front door path; load control = engine `statement_timeout` + caps; a configured row cap refuses loudly (§8a) instead of shortening results. |
| `ErrorResponse` (target) | Forwarded with raw `*pgconn.PgError` fields verbatim. | Never rewritten. |
| `ErrorResponse` (gate/front door) | Synthesized per §1.2 (§8a identity). | Distinguishable via `DETAIL` rule id. |
| `ReadyForQuery` | **Synthesized** from the ExecSession state machine (§6). Never emitted on `ErrTxOutcomeUnknown` (§6.3). | |
| `AuthenticationCleartextPassword`, `AuthenticationOk`, `BackendKeyData`, `ParameterStatus` (session-open set §3.3), `NegotiateProtocolVersion` (emitted per row 2.5 for major-3 minors and unrecognized `_pq_.*` options, then the session continues at 3.0) | Synthesized at startup. | `BackendKeyData` keys CSPRNG (MF7). |
| `CopyInResponse`/`CopyOutResponse`/`CopyBothResponse`, backend-direction `CopyData`/`CopyDone`, `NotificationResponse` (LISTEN refused), `FunctionCallResponse` (fast-path refused) | **Never emitted** — their triggers are all refused pre-target. | Asserted by conformance canaries: any of these arriving from the target = defect (classifier bypass), fatal + audited. |

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

### 6.5 Write-authority demotion

A session that retains read standing but loses write authority synchronously
rolls back any transaction opened under write authority before the next
ordinary or control unit can execute. The triggering unit is rejected rather
than silently continuing in autocommit; a confirmed rollback retains the
session at the reader floor, while cleanup failure transfers the attached
transaction to normal close. The slot ownership, race, audit, and failure
contract is normative in
[`synchronous-demotion-lifecycle.md`](synchronous-demotion-lifecycle.md).

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
| Major-3 minor > 0 and/or unrecognized `_pq_.*` options | **not a refusal**: `NegotiateProtocolVersion` (newest supported 3.0 + declined option names), session continues (row 2.5) | — | — |
| Unsupported protocol MAJOR (≠ 3) | uniform startup denial (`28000`), close | (audit-only) | — |
| GUC-setting startup `options` / ordinary unknown startup params / `replication` | uniform startup denial (`28000`) | (audit-only pre-auth) | — |
| `SET`-class statements post-auth | per ADR-0074 gate matrix (profile/role-gated; grammar GUCs always refused) | `gate/set-…` ids from 0074 §8a | — |
| Retained-state quota (configured 16 MiB/session or reservation failure) | `53400` (`configuration_limit_exceeded`, ruling 4) | `frontdoor/retained-budget` | Close unused prepared statements/portals, or raise the quota. Action: refuse statement; connection stays. |
| Global memory budget: input/output charge | backpressure, never an error (reads pause, audited) | `frontdoor/budget-backpressure` | — |
| Row-cap configured on a front-door connection / cumulative-output cap (8 GiB) | `54000` (`program_limit_exceeded`, ruling 4) | `frontdoor/no-silent-truncation` / `frontdoor/output-cap` | Remove the cap or use the RPC surface; the wire never shortens results. Action: abort statement; connection stays. |
| Bind parameters > 8192 | `54000` | `frontdoor/param-cap` | Action: refuse, discard-through-Sync. |
| Named statements > 256 / portals > 64 per session | `53400` | `frontdoor/named-object-cap` | Close unused objects. Action: refuse the `Parse`/`Bind`; connection stays. |
| Extended segment caps (10 000 msgs / 96 MiB before `Sync`) | `53400` | `frontdoor/segment-cap` | Issue `Sync` more often. Action: refuse, discard-through-Sync. |
| `SetMaxBodyLen` exceeded (declared length > 64 MiB) | `08P01` (fatal — cannot resynchronize a stream we refuse to read) | `frontdoor/message-too-large` | Action: error + close. |
| Partial-frame progress deadline (30s) | `08006` (`connection_failure`, fatal) | `frontdoor/frame-stall` | Action: close; audited. |
| Idle-in-tx timeout (90s / 10m debug) | FATAL `25P03` (`idle_in_transaction_session_timeout` — pg-conformant identity), tx rolled back + audited (which limit) | `gate/idle-in-tx-timeout` | Action: rollback + close per ADR-0074 timeout semantics. |
| Max-tx / idle-session (30m) deadlines | FATAL `57P05` shape (idle session) / §8a per ADR-0074 for max-tx | `gate/session-deadline` | Action: rollback if in-tx, close; audited. |
| Startup-phase caps (pre-auth 64 KiB msg, 64 conns, auth attempts, **lease/session caps at auth**) | uniform `28000`, close | internal audit identities only — incl. `lease-cap-exceeded`, `session-cap-exceeded` (ruling 4: wire stays uniform; audit carries the stable identity) | — |
| Accept-time capacity or throttle (rev 5) | **no frame** — the connection is closed before TLS, where a PostgreSQL error is unreadable bytes to a client waiting for `S`/`N` | `frontdoor/source-ip-throttled`, `frontdoor/connection-cap`, `frontdoor/pre-auth-connection-cap`, `frontdoor/control-lane-exhausted`, audited `fd.budget_refuse` | Action: close at accept, before the handler allocates. |
| A non-`p` frame in `S3` (rev 5) | uniform `28000`, close | `frontdoor/pre-auth-protocol-violation` — audited as a DENIAL and charged to the source, because sending a `Query` before authenticating is the peer's doing | Action: uniform denial + close; the frame never reaches the chain. |
| OUR store is unreachable during row 2.7 (rev 5) | uniform `28000`, close — telling a caller our database is down is an answer they have not earned either | `frontdoor/auth-store-error`, and deliberately **not** counted in the credential-attack trail nor charged to the source: the peer may have presented a perfectly good credential and been refused for our outage | Action: uniform denial + close; audited distinctly. |
| Post-auth message before the F1 slice ships (rev 5, temporary) | `0A000` (`feature_not_supported`) | `frontdoor/post-auth-not-implemented` | Action: accurate refusal + close. A silence here would look like a network fault and send someone debugging the wrong layer. Removed when F1 lands. |
| Out-of-state / unknown message | `08P01` (fatal) | `frontdoor/protocol-violation` | Action: error + close. |

Reader role: every statement wrapped read-only engine-side; write attempts
surface the target's `25006` verbatim (F3 proof paths).

---

## 8. Resource charging map (criteria 2 & 3)

### 8.1 Charge points (all charged BEFORE the resource exists)

| Path | Charged when | Lane | Released when |
|---|---|---|---|
| Message intake | header read: declared length validated (≤ 64 MiB) + charged before body read | general (control for §4-marked messages) | decode complete → either freed (transient) or transferred (below) |
| Extended segment accumulation | per message, counted against 10 000 msgs / 96 MiB | general | `Sync` (reset) or error-discard completion |
| Retained statements/portals | **reserved BEFORE the `Parse`/`Bind` is forwarded to the target** (r0 MF3); finalized (segment charge transfers to retained — no double-charge, no gap) at `ParseComplete`/`BindComplete`/`PortalSuspended`; reservation released on pre-Complete error | general | per the §4a object-release rules (Close + cascade, unnamed replacement, simple-Query destruction, transaction end for portals, session end) |
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
≈ 20 MiB) + control lane (20 MiB default; composes with max connections
per §1.4). Nothing allocates before charging; therefore the budget bounds
the aggregate regardless of per-object shape limits (whose naive product
is ~29 GiB — shape limits are NOT the bound).

### 8.5 Allocation-to-charge map (rev 5, lector PR #36 r0 must-fix 2)

Two constants in the implementation are both 64 KiB and both had comments
mentioning "TLS" and "decoder", which made them read as one charge taken
twice. They are not. **The coincidence in the numbers is what made this
worth writing down** — three terms above, and each is a different thing.

| §8.4 term | Constant | Charged against | Taken / released | What it covers |
|---|---|---|---|---|
| global budget | `exec.WireSessionOverhead` (64 KiB) | the ENGINE's resident budget, in row 2.7's atomic reservation | at session open / at session close | The **ExecSession's** fixed state: the session record, its registry and per-user entries, the reservation itself, and the per-session bookkeeping the engine keeps for the session's whole life. It is an admission reservation for a conservative fixed footprint, not allocator telemetry (lector's PR #33 ruling). **It does not cover any wire-side buffer.** |
| control lane | `frontdoor.ControlLanePerConn` (64 KiB) | the FRONT DOOR's control lane, at accept | at accept / at connection close | Reserved headroom so the messages that RELEASE the general budget can always be processed while it is saturated — `Sync`/`Flush`/`Terminate` intake, `ErrorResponse`/`NoticeResponse`/`ReadyForQuery` emission, cancel processing, teardown bookkeeping (§1.4). A reservation against a budget, not a description of an allocation. |
| per-connection fixed overhead | — | **nothing; bounded by the connection CAP** | — | The actual wire-side buffers: the `bufio.Reader`, the TLS record buffers, and the `pgproto3` backend's chunk reader. |

**The third row is a real statement about the current implementation and is
deliberately not a charge.** These are allocated per connection and bounded by
`MaxFrontendConns` — 320 × ≈64 KiB ≈ 20 MiB, which is exactly the figure §8.4
already carries. A charge would add nothing a hard connection cap does not
already guarantee: unlike segment input or retained state, this allocation
cannot grow with what a peer sends. If a future change makes any of these
buffers peer-sized, that stops being true and the row becomes a charge.

**Why the two reservations are the same number and stay independent:** each is
a conservative round figure for a different thing, arrived at separately. They
must not be collapsed into one constant on the strength of being equal today —
the engine's session footprint and the wire's control headroom answer to
different limits and will move for different reasons.

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
| **Output-stall deadline 30s** — one write of pending output to the client | every post-auth response write (§5 emissions, refusals, readiness). RULED (jarvis as lead, 2026-09-03, PR #52 Q1): with the 4 MiB watermark reached, a peer that has drained **nothing** for 30s is not a slow reader, it is a dead one; 30s sits above any sane TCP retransmit hiccup and below the point where held memory matters at the session cap. It is NOT an idle budget and does not inherit `idle`: idle measures a client that is not asking, this measures one that will not take what it asked for. Unbounded here let an authenticated client hold a session, the engine's one-in-flight claim, the pinned backend and its open transaction by selecting something large and not reading (PR #52 r1 MF7). |
| Cumulative output 8 GiB per statement (audited accounting cap) | `Execute`/`Query` result streaming |
| Bind params 8192 / 65535 | `Bind` |
| Named statements 256/1024, portals 64/256 per session | `Parse`/`Bind` named objects |
| Pre-auth message cap 64 KiB / 256 KiB | §2 rows 2.1–2.8 |
| Global budget 1 GiB / 4 GiB (+ control lane §1.4) | §8 |
| Pre-auth conns 64 / auth workers 16 | accept + §2 (the pre-auth slot is RETURNED at authentication, not at close: it bounds anonymous connections, and holding it for a session's life would let a few long-lived legitimate sessions consume the allowance that keeps half-open ones from starving them) |
| Auth attempts **1/conn**, **10 FAILED/min/IP** (rev 5) | §2 row 2.7 |
| `reserved_headroom` 4; `frontdoor_max_leases` derived ≥ 1 | the row-2.7 atomic reservation (row 2.9 acquires nothing; any later target checkout is NOT admission control and can neither race nor emit `fd.auth_ok` before capacity is held) |

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
  (header-first property: **a refused frame's body is never read to its
  declared length** — reads are bounded by the transport buffer, §8.3 — **and
  nothing past a refused header is interpreted as framing**).

  *Amended (jarvis as lead, 2026-09-03; Johno ratifies). The previous wording,
  "no read past a refused header", read as a source-read boundary the reader
  does not provide and does not need to. The measurement decided which side was
  wrong: a refusal consumed 4101 source bytes — one transport bufferful, NOT the
  frame's declared length. Had the reader been reading to the declared length, a
  refused oversized frame would cost megabytes. So the guarantee that has value
  already holds, and literal zero-read-ahead would buy nothing: the leftover
  bytes are bounded by the OS buffer, on a connection about to close, and
  reading exactly five header bytes per frame is a syscall per frame for no
  resource or correctness gain. This amends a spec to name the real guarantee;
  it does not relax a contract to match a defect.*
- Uniform-denial timing test (startup denials indistinguishable across
  causes — measured, not asserted).
- Never-emitted backend canaries (§5): CopyIn/CopyOut/CopyBothResponse,
  backend CopyData/CopyDone, NotificationResponse, FunctionCallResponse —
  classifier-bypass detectors, each individually asserted.

- **Specified-vocabulary enumeration** (criterion row; KB convention
  `shared/conventions/specified-vocabulary-enumeration.md`). Every mapper,
  guard, or state machine over a vocabulary this document specifies —
  the frontend and backend message catalogues, the canary set above, the
  startup parameter set — is enumerated from the SPECIFICATION and witnessed
  in code, never inferred from the paths a slice happens to reach. A
  deliberate omission is a row with its own identity and its own cell
  (forward / refuse-with-code / impossible-by-construction-and-why), never a
  `default:` arm that reads as impossible. Forged on two defects hours apart:
  a type-byte guard correct only for non-pipelining clients, and a backend
  mapper that knew none of the extended protocol's own replies. **A set stated
  in this document and nowhere else is a hand-maintained list wearing a spec's
  clothes** — the canary set above is bound to the code by
  `TestBackendCanaries_TheMatrixAndTheCodeAgree`, which fails if either side
  gains a member the other lacks.

## 11. Rulings on record (lector r0, 2026-08-31)

Rev 1's four open items were ruled in the r0 review; the rulings are
folded above and recorded here as binding:

1. **Fail closed at accept; never evict existing connections.** The
   lane/reservation arithmetic must compose (§1.4: lane ≥ max_conns ×
   per-conn reservation, validated at startup).
2. **Refuse non-UTF8 clients AND pin the target lease UTF8** (§3.1) —
   both ends verified, not assumed.
3. **`NegotiateProtocolVersion` for major-3 minors (downgrade to 3.0);
   refuse unsupported majors** (§2 rows 2.5/2.5a).
4. **`53400` for configured retained quotas; `54000` for row/output
   caps; startup lease-cap stays externally uniform `28000` with the
   internal stable `lease-cap-exceeded` audit identity** (§7).

Direct-TLS posture — RULED at r1 (lector, accepted): v1 refuses PG 17
`sslnegotiation=direct`/ALPN with an audited close and supports only
`SSLRequest` negotiation; default libpq clients are unaffected, clients
pinned to `direct` cannot connect until a later rev adds ALPN. No open
items remain.
