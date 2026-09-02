# The post-auth session loop — deadline and budget state machine

**Status:** F1. Companion to `docs/front-door/protocol-matrix.md`; every row here
cites the matrix line it implements. Written after PR #52 r0/r1 found twelve
findings of which eight were the same class — *which deadline is armed in which
state, and what is charged where*. That was not eight bugs, it was one model
nobody had written down, so each round found the next corner of it.

This document is the model. The loop is folded against it, and its cells cite it.

---

## 1. Why a connection needs more than one clock

A single "timeout" cannot express what the front door has to say, because the
four ways a connection stops making progress have different causes, different
blame, and different fixes:

| The peer… | is | budget | identity |
|---|---|---|---|
| has not started a message | idle | **idle** | `57P05` `gate/session-deadline` |
| started a message and stopped | slow / broken | **progress** | `08006` `frontdoor/frame-stall` |
| asked for a result and stopped reading it | not draining | **write** | transport close, `write-failed` |
| is waiting on a statement we are running | *fine* | **none** | — |

Collapsing any two of these produces a false operational record, and a false
record is worse than a missing one because somebody acts on it. Reporting the
front door's own idle deadline as `peer-closed` sends an operator to the client's
logs for a disconnection the server caused (r0 MF4). Reporting a half-sent
message as an idle session says the client went quiet when it went slow (r1 MF10).

**`net.Conn` deadlines are ABSOLUTE.** Every rule below follows from that: an
armed deadline is a wall-clock instant, not a sliding window, so "idle for 30m"
is only true if something re-arms it. Arming once at session open makes it a cap
on session *lifetime* (r0 MF2).

---

## 2. States, and the clock armed in each

| State | Deadline armed | On expiry | Set where |
|---|---|---|---|
| **pre-auth** (TLS, startup, credential) | `tls` / `startup` / `auth` | uniform denial, close | `startup.go`, `auth.go` |
| **idle — between messages** | `idle` (30m, §9) | `57P05` `gate/session-deadline` | top of the loop, **per message**, and only when the reader says the stream is between messages (§8.4 "refreshed on message/state transitions") |
| **frame in progress** (a type byte is present, body incomplete) | `frameStall` (30s, §7) | `08006` `frontdoor/frame-stall` | by `frameReader` as the type byte is read, **and at cycle entry when the stream is already mid-message** |
| **statement running** (engine owns the session) | **none** | — (engine's own statement/tx timeouts bound it) | cleared after a successful `Receive` |
| **output streaming** (each watermark flush) | `outputStall` per write | transport close, `write-failed` | `flushBounded`, cleared after each write |
| **extended segment held** (output reserved, awaiting the client's `Sync`) | `segmentStallBudget` (30s) | `08006` `frontdoor/segment-stall` | top of the loop, while the segment holds a reservation |
| **teardown / goodbye frame** | `deadlineGoodbyeBudget` (2s) | close regardless | in the expiry handlers |

Three consequences that were each a finding:

- The statement runs with **no** between-messages deadline (r1 lector) — a long
  legitimate result must not die under a budget named for idleness.
- Clearing it is **not** enough: the watermark's `Flush` is a blocking
  `conn.Write`, so clearing the deadline removed its only bound and a client that
  stopped reading held the session, the engine's one-in-flight claim, the pinned
  backend and its open transaction forever (r1 MF7). The **write** is bounded
  instead; the statement stays unbounded.
- The clear belongs after `Receive`, not on the Query arm: a session-surviving
  refusal also resolves the transaction status, and that is engine work too
  (r1 MF11).

**THE TYPE BYTE AND THE MESSAGE-START ARE OBSERVED ON THE BYTE PATH, NOT BESIDE
IT.** The first implementation handed the Backend a `*bufio.Reader` and peeked
one byte from that reader. That is correct for a client that sends a message and
waits for the reply — psql — and wrong for every client that PIPELINES, which is
lib/pq, JDBC batches, and every extended-protocol client by construction.
`pgproto3.NewBackend` wraps its reader in a chunkReader whose `Next` fills with
`io.ReadAtLeast(r.r, (*r.buf)[r.wp:], minReadCount)` — it takes the whole
available buffer, so message 2 is inside pgproto3 while the peeked reader is
empty, and the peek blocks on bytes already consumed. The second statement was
accepted by the client, never seen by the engine, and nobody was told: the
session hung until the idle deadline rather than closing. Found by white-vision
while wiring F2; it was live on `main`.

`frameReader` (`frame_reader.go`) tracks message boundaries on the only path the
bytes take, so it cannot be outrun by read-ahead.

**WHICH BUDGET IS OWED IS ASKED OF THE READER AT EVERY CYCLE ENTRY, NEVER
ASSUMED** (r0 MF1). Reporting the message-start as it is read is necessary and
not sufficient: the reader is shared with AUTH, and auth's Backend reads ahead
exactly as the session's does. A client writing its `PasswordMessage` and the
start of a `Query` in ONE write has that type byte consumed while the callback
is still nil — nothing can re-trigger it in the loop, so the half-sent message
waited under the budget for a client that is not asking. The entry check
(`fr.midMessage()`) covers a message already in flight; the callback covers one
that begins during a read. Both are needed, and the pair fails in both
directions: assuming idle loses the stall budget, and assuming frameStall
charges an idle session to a budget it never earned. It also makes the idle-vs-
mid-frame question a fact about the STREAM rather than an inference from whether
a peek had succeeded — which is strictly better than what it replaced. One trap
found while building it: clearing the length counter only when the next type
byte arrived left every COMPLETED message looking like one in progress, so the
first idle session after any message was audited as a frame stall.

**The goodbye frame needs its own write budget.** The deadline that fired bounds
writes as well as reads, so the frame explaining an expiry cannot be written
under it — the client gets a bare EOF, which is exactly the "looks like a network
fault" outcome an accurate SQLSTATE exists to prevent.

---

## 3. What is charged, and when

| Budget | Value | Charged | Matrix |
|---|---|---|---|
| pending serialized output | 4 MiB watermark | **before** serialization of each frame; flush, audited `fd.backpressure_enter`/`_exit` | §8.4 :387 |
| cumulative statement output | 8 GiB | per frame, across the whole statement; stops FORWARDING with `54000` `frontdoor/output-cap`, connection survives. **It does not undo the statement** — see §4 | §7 :358 |
| control lane | 64 KiB/conn | at accept | §1.4 |
| **general lane (process-wide)** | 1 GiB | the statement's output **working set** (= the watermark) reserved **BEFORE DISPATCH**; topped up mid-statement only for a single frame larger than the reservation; released on every statement exit; saturation is backpressure, audited `frontdoor/budget-backpressure` | §1.4, §8.1 |
| wire-side buffers (`bufio.Reader`, TLS records, pgproto3 chunk reader) | — | **not charged**; bounded by `MaxFrontendConns` because they cannot grow with what a peer sends | §8.4 third term |

Two rules that were findings:

- **Charge before serialization, not after.** Counting after `Send` means the
  buffer has already grown by the frame that crossed the line, so the watermark
  fires one frame late — and one frame can be large (r1 MF8).
- **Every message kind counts.** The estimate once looked only at `Values`,
  `Fields` and `Tag`, so a burst of large `NoticeResponse` or a fat target
  `ErrorResponse` filled the buffer while crossing no watermark at all. Notices
  and errors carry their whole payload in their fields (r1 MF8).

The size is an **estimate**: pgproto3 exposes no way to ask the Backend how much
it holds, and encoding twice to measure would double the work on the hot path.
Payload dominates framing overhead for the messages that can actually grow a
buffer, so an estimate that ignores a few header bytes is wrong by a margin that
does not matter to a 4 MiB bound.

---

## 4. Which error identity, and why they must stay distinct

| Situation | SQLSTATE | DETAIL | Connection |
|---|---|---|---|
| idle past budget | `57P05` | `gate/session-deadline` | closed |
| partial frame past budget | `08006` | `frontdoor/frame-stall` | closed |
| statement output past 8 GiB | `54000` | `frontdoor/output-cap` | **survives** — and the message says the statement EXECUTED |
| extended segment left open past budget | `08006` | `frontdoor/segment-stall` | closed |
| gate refusal (classifier, grants, size) | per §7 | `gate/…` | survives, **readiness follows** |
| target error | the target's own, verbatim | the target's own | survives, readiness follows |
| fast-path, extended frames, COPY, unknown byte | §7 / Johno's ruling | `frontdoor/…` | per row |
| client stopped reading | — (no frame reaches it) | — | closed, `write-failed` |
| a frame the front door cannot build | `08P01` | `frontdoor/protocol-violation` | closed |

**A segment left open is not an idle session.** `frontdoor/segment-stall` is
frame-stall's sibling one level up — a whole segment half-open rather than one
frame — hence the same `08006` class. It is deliberately NOT `57P05`
`gate/session-deadline`: that reason says the session was idle, and this client
was not idle, it asked for output and then did not collect it. It is not
`write-failed` either, because the peer may well be reading; it simply never
ended its segment. The budget is its OWN constant at 30s and does not inherit
`idle` or `generalLaneWaitBudget` — per §5.1, three budgets that happen to share
a number are three MEANINGS, and sharing a constant would make a later change to
one silently change the others.

It arms ONLY while a segment holds a lane reservation. A segment that has Parsed
and Bound but executed nothing has produced no output, holds nothing, and is a
client merely between messages: it stays under the idle clock, because adding a
second timer to an idle session buys nothing and costs a state.

**The lane can span a client gap by design; the budget bounds the gap, it does
not remove it.** The alternative was to flush at the end of every streaming call,
which would keep the lane out of every gap — and it is rejected because it sends
bytes earlier than PostgreSQL would. Fidelity to the application outranks a
tidier accounting story (Amendment 6): the requirement is that a wire session
behaves as PostgreSQL does, and PostgreSQL does not send until `Flush` or `Sync`.

**Wire identity follows the published §7 catalogue; audit identity follows the
defect.** Two decisions may share a wire `DETAIL` when §7 names one id for their
class — an operator still has to tell them apart, so the *audit* reason is what
must not collide (r0, self-corrected).

**Never report a refusal for an effect that happened.** A budget that stops
forwarding does so while output is being FORWARDED — after the target ran the
statement and, in an implicit block, after those effects committed. Reporting
that as a refusal is a lie the client acts on: r2 returned `54000` over a hundred
durable rows, and r4 found the identical lie at the general lane, in the function
r2 had just fixed.

Twice at two sites is a pattern, and a third would be a design smell (jarvis,
r4), so the fold is **structural** rather than a fix per budget:

1. **Prevent before the effect where the budget can be known.** A statement's
   output working set is bounded by the watermark, so it is reserved from the
   process lane *before dispatch*. The ordinary saturation case — a busy process,
   another connection arriving — is then refused at a point where refusing is
   simply TRUE: nothing executed, nothing is durable, `fd.refused` is the correct
   audit. This is also far less lane traffic than a reservation per frame. For a
   simple `Query` the *output size* is unknown before execution, so post-effect
   withholding remains the only honest shape for what the reservation cannot
   cover — a single frame larger than the whole working set.
2. **One post-dispatch stop path, and nowhere else to land.** `outputWithheld`
   is a closed set; a site names a reason and nothing else about the story is its
   to tell. `reportOutputWithheld` is a **stage, not a helper**: it derives the
   wire identity and the "what stopped" clause from `withheldReasons`, and it
   asks the ENGINE what became of the effects. No site composes its own message.
3. **The effects clause is the engine's answer, not the budget's — and `I` is
   not an answer.** (r5 MF16.) Every earlier
   version asserted *"the statement's effects are committed"* — which is FALSE
   inside an explicit `BEGIN`, where they are pending and a `ROLLBACK` still
   decides them, and a client that believes it may skip the `COMMIT` that would
   have made it true. `WireTxStatus` already answers the ok / pending_commit /
   aborted trichotomy, so that is what the client and the audit are told. The
   same read serves the readiness byte, deliberately: asking twice could answer
   differently, and an error text that disagrees with the readiness byte about
   the transaction is the very inconsistency this path exists to prevent.

   But an **idle** session is what a committed autocommit leaves behind *and*
   what a failed, rolled-back one leaves behind, so `I` alone establishes
   nothing. `recordedEffects` therefore answers only from what is known, in this
   order:

   | Observation | Outcome | Certainty |
   |---|---|---|
   | the target's error passed through the emitter | `failed`, effects rolled back | seen |
   | `T` — transaction still open | `pending_commit` | certain |
   | `E` — transaction aborted | `aborted` | certain |
   | `I`, nothing observed | `unresolvable` | **not known** |

   The last row is the honest one: stopping early is the whole premise of this
   path, so the front door stopped reading *before* the target reported the
   outcome, and "committed" is not a conclusion available to it however likely it
   is. A cell proves the rows are in fact there and asserts the weaker word
   anyway. Jarvis's `EmitStopped` seam will carry the engine's own observation of
   the drained tail and let the first three rows cover the fourth; until then
   "unresolved" is what the front door can say without inventing.

The audit records `fd.stmt_outcome`, never `fd.refused`, for anything that ran,
so the operational record and the database agree.

**Every session-surviving refusal ends its cycle with `ReadyForQuery`.** §6.3
names the only exception: an unknown transaction outcome. A client that waits for
readiness — libpq's `PQfn`, and the large-object interface on it — blocks forever
without it, and a raw `pgproto3.Frontend` never waits, which is why a cell must
be written to wait on purpose (r0 MF1).

---

## 5. Open questions for a ruling

1. ~~`outputStall` is not a matrix number.~~ **RULED** (jarvis as lead,
   2026-09-03, PR #52 Q1): 30s stands, and it is now a row in the matrix's §8.4
   budget table with its rationale — with the watermark full, a peer that has
   drained nothing for 30s is a dead reader, not a slow one. It does NOT inherit
   `idle`: idle measures a client that is not asking, this measures one that will
   not take what it asked for.
2. ~~No global resident-output budget.~~ **BUILT** (PR #52 r3, lector MF8). The
   general lane (`general_lane.go`, 1 GiB per §1.4) now carries pending
   serialized output: reserved before serialization, released when the bytes
   reach the socket, released again on EVERY exit from a statement per §8.2. The
   per-connection watermark paces one connection; only the lane can stop a
   thousand of them each holding 4 MiB from adding up past the process budget.
   Saturation is BACKPRESSURE, never an error (§7): flush first — itself a
   release — then wait for another connection to release. One policy figure
   remains un-pinned by the matrix: how long a connection waits before it stops
   holding a statement open (`generalLaneWaitBudget`, 30s, §5.2). Unbounded waiting on
   a lane nothing releases is a hung session holding the engine's claim and a
   pinned backend, which is r1 MF7 in different clothing.
   **r4 restructured where the lane is charged** (see §4): the working set is
   reserved before dispatch rather than frame by frame, which moves ordinary
   saturation to a pre-effect refusal and leaves the post-dispatch path only the
   oversized-frame top-up it cannot avoid.
3. **A cell may not synchronize on a proxy for the lane** (r5 MF17). `runQuery`
   releases its reservation in a `defer` that runs *after* the response is
   flushed, so a client holding `ReadyForQuery` does not imply an idle lane. A
   probe measured the window directly: on **22 of 300** statements the lane still
   held its 128-byte working set at the moment the client had already been told
   the statement finished. Anything that occupies the lane must wait on
   `inUse() == 0`, never on the previous statement's readiness.
4. **The watermark has no time-based flush.** Within contract — the matrix
   specifies a size watermark only — but a slow-producing statement shows the
   client nothing until 4 MiB accrues or it ends, so an interactive client on a
   slow query looks hung. Size-or-time is the usual answer.
