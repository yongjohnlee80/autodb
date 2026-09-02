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
| **idle — between messages** | `idle` (30m, §9) | `57P05` `gate/session-deadline` | top of the loop, **per message** (§8.4 "refreshed on message/state transitions") |
| **frame in progress** (a type byte is present, body incomplete) | `frameStall` (30s, §7) | `08006` `frontdoor/frame-stall` | after `Peek` succeeds, before `Receive` |
| **statement running** (engine owns the session) | **none** | — (engine's own statement/tx timeouts bound it) | cleared after a successful `Receive` |
| **output streaming** (each watermark flush) | `outputStall` per write | transport close, `write-failed` | `flushBounded`, cleared after each write |
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
| **general lane (process-wide)** | 1 GiB | pending output reserved BEFORE serialization, released when flushed AND on every statement exit; saturation is backpressure, audited `frontdoor/budget-backpressure` | §1.4, §8.1 |
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
| gate refusal (classifier, grants, size) | per §7 | `gate/…` | survives, **readiness follows** |
| target error | the target's own, verbatim | the target's own | survives, readiness follows |
| fast-path, extended frames, COPY, unknown byte | §7 / Johno's ruling | `frontdoor/…` | per row |
| client stopped reading | — (no frame reaches it) | — | closed, `write-failed` |
| a frame the front door cannot build | `08P01` | `frontdoor/protocol-violation` | closed |

**Wire identity follows the published §7 catalogue; audit identity follows the
defect.** Two decisions may share a wire `DETAIL` when §7 names one id for their
class — an operator still has to tell them apart, so the *audit* reason is what
must not collide (r0, self-corrected).

**Never report a refusal for an effect that happened.** The output cap trips
while a statement's output is being forwarded — after the target ran it, and for
DML in an implicit block after those effects committed. Reporting that as a
refusal is a lie the client acts on: r2 saw `54000` returned while 100 rows were
committed. So the cap says the statement EXECUTED and its output was withheld,
and the audit records `fd.stmt_outcome`, never `fd.refused`, so the operational
record and the database agree. The place to PREVENT the effect is the
retained-capacity reservation before dispatch; once the rows exist, honesty is
the only remedy this path has (r2 MF9, reframed by jarvis).

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
   holding a statement open (`generalLaneWaitBudget`, 30s). Unbounded waiting on
   a lane nothing releases is a hung session holding the engine's claim and a
   pinned backend, which is r1 MF7 in different clothing.
3. **The watermark has no time-based flush.** Within contract — the matrix
   specifies a size watermark only — but a slow-producing statement shows the
   client nothing until 4 MiB accrues or it ends, so an interactive client on a
   slow query looks hung. Size-or-time is the usual answer.
