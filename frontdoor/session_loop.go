package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE POST-AUTH SESSION LOOP (F1's wire half).
//
// It reads frontend frames, routes them, and frames what comes back. It executes
// nothing: a Query goes to exec.WireQuery, which owns classification, the gate,
// the F3a unit policy and dispatch as ONE operation over the same bytes, and
// hands back neutral messages plus a transaction status. Everything decidable
// from the frame alone was already decided in session_dispatch.go.
//
// That leaves this file with one job — turning engine vocabulary into wire
// frames — and it is the only place in the front door that does so for a
// statement's results.

// runSession is the SessionHandler that replaces defaultSession.
//
// fr sits BETWEEN the socket and the Backend and validates framing as bytes
// pass, so the type-byte check happens before decode without the loop and the
// Backend sharing a reader (frame_reader.go).
//
// The earlier design DID share one — a *bufio.Reader the loop peeked beside the
// Backend — and this comment described it. It was correct for a client that
// sends one message and waits, and wrong for every client that pipelines: pgx's
// chunkReader reads ahead, so the peek blocked on bytes pgproto3 had already
// taken (PR #59). Lector flagged the surviving comment as non-blocking on #59
// r1; it is corrected here rather than on that PR's approved head.
func (l *Listener) runSession(ctx context.Context, conn net.Conn, fr *frameReader,
	be *pgproto3.Backend, sess exec.WireSessionResult, peer string, closeReason *string) error {

	// A MESSAGE HAS STARTED — reported from the byte path, because once pgproto3
	// may have read ahead that is the only place the fact is observable. From
	// here the peer is mid-frame, which §7 gives its own budget and its own
	// identity: a peer that stops halfway through a message is not idle, and
	// reporting a frame stall as an idle timeout tells an operator the client
	// went quiet when it actually went slow.
	//
	// Installed ONCE, not per iteration. It covers a frame that BEGINS during a
	// read; a frame already in progress when a cycle starts is covered by the
	// entry check below, because the callback cannot fire twice for one message
	// and a message read ahead during auth has already spent its start.
	fr.setOnStart(func() {
		_ = conn.SetDeadline(l.now().Add(l.dl.frameStall))
	})

	// THE SEGMENT'S LANE RESERVATION IS OWNED HERE, not by the frame that took
	// it, because an extended segment spans many frames and many trips round this
	// loop. §8.2 is release on EVERY path: Sync is the segment's normal exit, but
	// a client that vanishes between Execute and Sync never sends one, and a
	// release-at-Sync design would leak those bytes for the life of the process.
	// This defer is the only thing that runs on every way out of a session.
	var seg segmentLane
	defer seg.release(l)

	for {
		// WHICH BUDGET IS OWED IS A QUESTION ABOUT THE STREAM, and the reader is
		// the only thing that can answer it — so it is ASKED here rather than
		// assumed (r0 MF1).
		//
		// Arming idle unconditionally was wrong for a message already in flight.
		// The reader is shared with auth, and auth's Backend reads ahead exactly
		// as this one does: a client writing its PasswordMessage and the start of
		// a Query in ONE write has its type byte consumed while the message-start
		// callback is still nil, so nothing can re-trigger it here and a half-sent
		// message waited under the budget for a client that is not asking.
		//
		// THE IDLE BUDGET IS RE-ARMED PER MESSAGE (matrix §8.4: "refreshed on
		// message/state transitions"). net.Conn deadlines are ABSOLUTE, so a
		// single arming at session open would make the idle budget a cap on
		// session LIFETIME — a pooled connection doing steady work would die
		// mid-statement at thirty minutes, having never once been idle.
		// A SEGMENT HOLDING OUTPUT GETS ITS OWN CLOCK. Between Execute and Sync the
		// peer owes us a Sync and we are holding its bytes, so the 30-minute idle
		// budget is the wrong measure — see segmentStallBudget. A HALF-SENT
		// MESSAGE still wins over both: a peer stopped mid-frame is neither idle
		// nor sitting on a segment, and §7 gives that its own identity.
		budget := l.segmentDeadline(&seg)
		if fr.midMessage() {
			budget = l.dl.frameStall
		}
		if err := conn.SetDeadline(l.now().Add(budget)); err != nil {
			*closeReason = "deadline"
			return nil
		}

		msg, err := be.Receive()
		if err != nil {
			return l.endOfRead(conn, be, fr, &seg, err, peer, closeReason)
		}

		// The frame is in. Engine work is NOT between-messages time, and it is
		// not only Query that does some: a session-surviving refusal resolves the
		// transaction status too. Clearing here rather than at the Query arm
		// covers every path that leaves this loop for the engine.
		if err := conn.SetDeadline(time.Time{}); err != nil {
			*closeReason = "deadline"
			return nil
		}

		// DISCARD-THROUGH-SYNC APPLIES TO EVERY MESSAGE, and it has to be here —
		// before the decision table, before the extended routing, before Query —
		// because PostgreSQL's rule is about the SEGMENT, not about one protocol's
		// frames. It ignores everything but Sync and Terminate after an error in a
		// segment, and a simple Query is a message like any other.
		//
		// Guarding only the extended path let a Query pipelined behind a refused
		// Parse fall through to runQuery and EXECUTE: lector r1 MF4 committed a
		// row that way. A client that mixes the protocols on one connection —
		// lib/pq does — hits that with ordinary traffic.
		if seg.discarding {
			switch msg.(type) {
			case *pgproto3.Sync:
				// Ends the discard; routed below so the engine clears the segment
				// and the client gets its readiness byte.
			case *pgproto3.Terminate:
				// Always admissible, and the decision table below closes cleanly.
			default:
				continue // discarded, exactly as the target would discard it
			}
		}

		if d, decided := dispatchFrame(msg); decided {
			l.applyDispatch(conn, be, d, peer, closeReason)
			if d.after == endSession {
				return nil
			}
			// A refusal that KEEPS the session still ends a protocol cycle, and
			// every cycle ends with readiness. PostgreSQL's own Function Call
			// sub-protocol says so explicitly — ReadyForQuery is sent "whether
			// processing terminates successfully or with an error" — and §6.3
			// names the ONLY case where readiness is withheld, which is an
			// unknown transaction outcome, not this.
			//
			// Without it a client that WAITS for readiness blocks forever:
			// libpq's PQfn does, and so does the large-object interface built on
			// it. A raw Frontend does not wait, which is exactly why the loop's
			// own cell could not see this and a real client would.
			if !l.sendReadiness(conn, be, sess, peer, closeReason) {
				return nil
			}
			continue
		}

		// THE EXTENDED FRAMES ARE ROUTED, not decided — see session_extended.go
		// for why they cannot be a dispatchFrame entry. One loop, never two.
		if extendedFrame(msg) {
			if !l.runExtended(ctx, conn, be, sess, msg, peer, &seg, closeReason) {
				return nil
			}
			continue
		}

		q, ok := msg.(*pgproto3.Query)
		if !ok {
			// Every defined type byte is either decided by the frame table or is
			// a Query. A frame reaching here means the two have drifted apart —
			// report it rather than ignoring the frame, which would leave the
			// peer waiting for a reply that is never coming.
			d := unknownMessageType()
			l.applyDispatch(conn, be, d, peer, closeReason)
			return nil
		}
		if !l.runQuery(ctx, conn, be, sess, q.String, peer, closeReason) {
			return nil
		}
	}
}

// endOfRead decides what a failed read means. A read fails for two reasons that
// deserve different answers: the peer went away, or OUR OWN deadline fired.
//
// Recording both as "peer-closed" makes the audit say the client hung up when
// the front door in fact hung up on it — and a false operational record is worse
// than a missing one, because someone will act on it. §7 gives the deadline case
// its own identity: a FATAL 57P05 under gate/session-deadline.
func (l *Listener) endOfFrameRead(conn net.Conn, be *pgproto3.Backend, err error, peer string, closeReason *string) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		// Mid-frame, not idle: §7's progress budget, with its own SQLSTATE. The
		// write needs its own budget for the same reason the idle path does —
		// the deadline that fired bounds writes too.
		_ = conn.SetDeadline(l.now().Add(deadlineGoodbyeBudget))
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleFrameStall, Peer: peer})
		be.Send(gateError("FATAL", sqlStateConnectionFailure,
			"terminating connection: a message stalled part-way through", ruleFrameStall,
			"the peer began a message and stopped; the front door does not wait indefinitely mid-frame"))
		_ = l.flushBounded(conn, be)
		*closeReason = ruleFrameStall
		return nil
	}
	*closeReason = "peer-closed"
	return nil
}

// endOfIdleWait decides what a failed wait for the NEXT message means.
// endOfRead answers one failed Receive.
//
// It replaces the split between an idle wait and a frame read, which the loop
// used to make by knowing whether its peek had succeeded. With framing checked
// on the byte path, the READER is what knows: it is mid-message or it is not,
// and that is a fact about the stream rather than an inference from control
// flow. The two framing faults it can report are answered here too, before the
// timeout arms, because they are not timeouts.
func (l *Listener) endOfRead(conn net.Conn, be *pgproto3.Backend, fr *frameReader,
	seg *segmentLane, err error, peer string, closeReason *string) error {

	switch {
	case errors.Is(err, errUnknownFrameType):
		// The distinction the peek existed for, preserved: an undefined type
		// byte is told apart from a transport failure, and gets the accurate
		// 08P01 rather than a silent close. The byte itself goes to the audit,
		// never to the wire (§1.2).
		// The REASON stays the stable rule id — an audit trail is greppable only
		// if its identities do not vary — and the offending byte rides in the
		// detail, where a varying value belongs.
		d := unknownMessageType()
		d.auditDetail = fmt.Sprintf("undefined frontend message type 0x%02x", fr.badByte())
		l.applyDispatch(conn, be, d, peer, closeReason)
		return nil

	case errors.Is(err, errBadFrameLength):
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleProtocolViolation, Peer: peer,
			Detail: "a frontend message declared a length shorter than its own length field"})
		be.Send(gateError("FATAL", sqlStateProtocolViolation,
			"invalid message length", ruleProtocolViolation,
			"the length field counts itself; a value under four describes no message"))
		_ = l.flushBounded(conn, be)
		*closeReason = "protocol-violation"
		return nil
	}

	if fr.midMessage() {
		return l.endOfFrameRead(conn, be, err, peer, closeReason)
	}
	// Nothing has started, so whose budget expired depends on what the session
	// was waiting FOR. A segment holding a reservation was waiting for a Sync;
	// reporting that as an idle timeout would send an operator looking for a
	// client that had gone quiet when it had not.
	if seg != nil && seg.held > 0 {
		return l.endOfSegmentWait(conn, be, err, peer, closeReason)
	}
	return l.endOfIdleWait(conn, be, err, peer, closeReason)
}

func (l *Listener) endOfIdleWait(conn net.Conn, be *pgproto3.Backend, err error, peer string, closeReason *string) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		// The deadline that expired is the CONNECTION's, and it bounds writes as
		// well as reads — so the goodbye frame cannot go out under it. Give the
		// write its own short budget: without this the client is told nothing and
		// sees a bare EOF, which is the very "looks like a network fault" outcome
		// the accurate SQLSTATE exists to prevent.
		_ = conn.SetDeadline(l.now().Add(deadlineGoodbyeBudget))
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleSessionDeadline, Peer: peer})
		be.Send(gateError("FATAL", sqlStateIdleSessionTimeout,
			"terminating connection due to idle timeout", ruleSessionDeadline,
			"the front door closes a session that has been idle past its budget"))
		_ = l.flushBounded(conn, be)
		*closeReason = ruleSessionDeadline
		return nil
	}
	// The peer stopped talking. Nothing to report to a socket that is already
	// gone.
	*closeReason = "peer-closed"
	return nil
}

// sendReadiness ends a protocol cycle with the session's current transaction
// state. It reports whether the session continues.
func (l *Listener) sendReadiness(conn net.Conn, be *pgproto3.Backend, sess exec.WireSessionResult,
	peer string, closeReason *string) bool {

	status, err := l.queries.WireTxStatus(sess.SessionID, sess.UserID)
	if err != nil || !validTxStatus(status) {
		*closeReason = "session-lost"
		return false
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// flushBounded writes what the Backend is holding under an OUTPUT-STALL budget.
//
// It exists because clearing the between-messages deadline for the statement's
// duration — right, so a long legitimate result is not killed by an IDLE budget —
// removed the only bound on the watermark's flush, and be.Flush is a BLOCKING
// conn.Write. A client that stops reading fills its window and then our send
// buffer, and that write parks forever, holding the session goroutine, the
// engine's one-in-flight claim, the pinned backend and its open transaction. An
// authenticated client needed only to select something large and stop reading.
//
// The engine's statement and transaction timeouts do NOT bound this: the target
// already produced the rows and is executing nothing. The wait is autodb → client,
// on a socket, and conn.Write does not watch a context.
//
// So the WRITE is bounded and the between-messages budget stays cleared: a slow
// legitimate statement still runs unbounded, and only a client that will not take
// what it asked for is cut off.
func (l *Listener) flushBounded(conn net.Conn, be *pgproto3.Backend) error {
	if err := conn.SetWriteDeadline(l.now().Add(l.dl.outputStall)); err != nil {
		return err
	}
	err := be.Flush()
	// Cleared, never left armed: SetWriteDeadline is absolute, and a stale one
	// would fail the NEXT write for a stall that already ended.
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// outputCap is §7's cumulative per-statement bound, or a cell's lowered one.
func (l *Listener) outputCap() int64 {
	if l.testOutputCap != nil {
		return *l.testOutputCap
	}
	return cumulativeOutputCap
}

// outputWatermark is §8.4's bound on serialized output waiting to be written,
// or a cell's lowered one.
func (l *Listener) outputWatermark() int64 {
	if l.testWatermark != nil {
		return *l.testWatermark
	}
	return pendingOutputWatermark
}

// laneWait is how long a statement waits for the general lane, or a cell's
// shortened budget. The figure itself is policy (session-loop-budgets.md §5.2).
func (l *Listener) laneWait() time.Duration {
	if l.testLaneWait != nil {
		return *l.testLaneWait
	}
	return generalLaneWaitBudget
}

// outputWithheld names WHY the front door stopped forwarding output for a
// statement that had ALREADY BEEN DISPATCHED. It is a closed set, and adding a
// budget means adding a row to withheldReasons below — which is the point.
//
// This type exists because the same defect was found twice at two sites (r2 MF9
// at the cumulative cap, r4 MF15 at the general lane), and a third site would
// have been a third chance to get it wrong. A stop reason can no longer compose
// its own account of what happened to the statement: it names what stopped, and
// the stage asks the ENGINE what became of the effects (jarvis, r4).
type outputWithheld int

const (
	// outputComplete is the zero value: nothing was withheld. It is first so
	// that a site which forgets to name a reason reports nothing rather than
	// silently picking whichever constant happens to be zero.
	outputComplete outputWithheld = iota
	withheldAtCap
	withheldOnSaturatedLane
)

// withheldReasons maps each stop reason to its §7 wire identity, the clause
// naming WHAT STOPPED, and the operator's remedy for it.
//
// Note what is NOT here: any claim about the statement's effects. A budget knows
// why forwarding stopped; only the engine knows whether the rows are durable,
// and every version of this code that let the budget answer both questions got
// the second one wrong.
var withheldReasons = map[outputWithheld]struct{ rule, stopped, remedy string }{
	withheldAtCap: {
		rule:    ruleOutputCap,
		stopped: "its output exceeded the session's output budget",
		remedy:  "narrow the result, or use the RPC surface to read it",
	},
	withheldOnSaturatedLane: {
		rule:    ruleBudgetBackpressure,
		stopped: "the server's output budget was saturated",
		remedy:  "retry the read when the server is less busy",
	},
}

// errStopForwarding unwinds the emitter once a stop reason has been recorded.
// One sentinel for every reason, because the reason is carried by the
// outputWithheld value and the post-dispatch handling is a single branch.
var errStopForwarding = errors.New("frontdoor: forwarding stopped by a budget")

// runQuery executes one simple-query buffer and frames the response. It reports
// whether the session continues.
func (l *Listener) runQuery(ctx context.Context, conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, sql, peer string, closeReason *string) bool {

	// The emitter FRAMES AND RETURNS. It must not call back into the engine on
	// this session: WireQuery holds the session's one-in-flight claim across
	// every emit, so a re-entrant call gets ErrSessionBusy by design. Anything
	// this loop needs from the engine happens after WireQuery returns.
	// PRE-DISPATCH RESERVATION — refuse before the effect where the budget can be
	// known (jarvis, r4: "it is the cheaper truth").
	//
	// A statement's output working set is bounded by the watermark, so it can be
	// reserved from the process lane BEFORE the target runs anything. That moves
	// the ordinary saturation case — a busy process, a connection arriving into
	// it — to a point where refusing is simply TRUE: nothing has executed, no
	// rows are durable, and the client can retry with nothing left behind. It is
	// also far less lane traffic than a reservation per frame.
	//
	// What it cannot cover is a single frame larger than the whole reservation;
	// that still tops up mid-statement, and if the lane cannot admit it the
	// statement has already run. Hence the post-dispatch stage — which now sees
	// the rare case rather than the common one.
	held := l.outputWatermark()
	if cap := l.general.capacity(); held > cap {
		held = cap
	}
	if !l.general.reserve(held, l.laneWait(), l.now) {
		// Backpressure, not a defect (§7) — and honest, because it is PRE-effect.
		// This is the one place in the statement path where fd.refused is the
		// truthful audit for a budget: nothing ran.
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleBudgetBackpressure, Peer: peer,
			Detail: "the general lane could not admit this statement's output working set; nothing was dispatched"})
		be.Send(gateError("ERROR", sqlStateProgramLimit,
			"the server is at its output budget and did not run this statement", ruleBudgetBackpressure,
			"nothing was executed; retry when the server is less busy"))
		if ferr := l.flushBounded(conn, be); ferr != nil {
			*closeReason = "write-failed"
			return false
		}
		return l.sendReadiness(conn, be, sess, peer, closeReason)
	}
	// The emitter SEES the target's error before deciding whether to forward it,
	// so a failure that arrives before the stop is observed rather than inferred
	// (r5 MF16). A failure arriving AFTER the stop is not observable here at all
	// — that is the gap jarvis's EmitStopped seam closes.
	acct := newOutputAccountant(l, conn, be, peer, held)
	// RELEASE ON EVERY PATH (§8.2): whatever this statement holds goes back when
	// it returns, however it returns — and it reads acct.held, not the figure
	// reserved above, so an oversized frame's top-up cannot leak.
	defer func() { l.general.release(acct.held) }()
	status, err := l.queries.WireQuery(ctx, sess.SessionID, sess.UserID, sql, hostOf(peer),
		acct.emit)

	// The engine wraps the emitter's own error, so errors.Is on errStopForwarding
	// keeps working while errors.As lifts the report out of it.
	var stopped *exec.EmitStopped
	if err != nil {
		var st *exec.EmitStopped
		if errors.As(err, &st) {
			stopped = st
		}
	}

	switch {
	case acct.withheld != outputComplete:
		// EVERY post-dispatch stop lands here, and there is nowhere else for one
		// to land. The cap and the lane trip for different reasons at different
		// sites; what is owed to the client and the audit is identical, so it is
		// decided once, below, from what the engine records.
		// A simple Query owns its own terminal readiness.
		return l.reportOutputWithheld(conn, be, sess, peer, closeReason, acct.withheld, stopped, acct.targetFailed, true)

	case acct.writeErr != nil:
		// The peer is gone or will not read. Nothing to send it, and nothing
		// here is anyone's defect.
		*closeReason = "write-failed"
		return false

	case acct.emitErr != nil:
		// A message the front door cannot frame is a defect on OUR side, not the
		// peer's — a target emitting something impossible (§5's never-emitted
		// canaries) lands here. Say so accurately and close; forwarding a guess
		// would be worse than stopping.
		l.onEvent(Event{Kind: "fd.refused", Reason: unframeableAudit(acct.emitErr), Peer: peer, Detail: acct.emitErr.Error()})
		be.Send(gateError("FATAL", sqlStateProtocolViolation,
			unframeableMessageText(acct.emitErr), ruleProtocolViolation,
			unframeableHint(acct.emitErr)))
		_ = l.flushBounded(conn, be)
		*closeReason = "unframeable-message"
		return false

	case err != nil:
		return l.frameGateError(conn, be, sess, err, peer, closeReason)
	}

	if !validTxStatus(status) {
		// The status is a claim the client acts on — psql prints it, a driver
		// decides whether to send COMMIT from it — so an unrecognized byte is
		// never forwarded.
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleUnframeableMessage, Peer: peer,
			Detail: fmt.Sprintf("transaction status %q", status)})
		*closeReason = "invalid-tx-status"
		return false
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	if err := l.flushBounded(conn, be); err != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// reportOutputWithheld is the ONE post-dispatch stop path: the single place
// that speaks for a statement whose output the front door stopped forwarding
// after the target had already run it.
//
// Every such stop trips while output is being FORWARDED — after the statement
// executed and, in an implicit block, after its effects committed. Reporting
// that as a refusal is a lie the client acts on: r2 returned 54000 over a
// hundred durable rows, and r4 found the identical lie at the lane, in a
// function I had just fixed for the cap. So this is a STAGE, not a helper the
// sites call with their own prose (jarvis, r4): a site names a reason, and
// nothing else about the story is its to tell.
//
// THE EFFECTS CLAUSE COMES FROM THE ENGINE, NOT FROM THE BUDGET. Whether the
// rows are durable depends on the transaction the statement ran in, which the
// budget cannot know and every earlier version of this text simply asserted:
// "the statement's effects are committed" is FALSE inside an explicit BEGIN,
// where they are pending and a ROLLBACK still decides them. The transaction
// phase is what the engine records, so it is what the client is told.
//
// The status read here also serves the readiness byte, deliberately: asking
// twice could answer differently, and a session whose error text and readiness
// byte disagree about its transaction is exactly the inconsistency this whole
// path exists to prevent.
//
// PREVENTING the effect beats reporting it, and where a budget CAN be known
// before dispatch it is charged there instead — see the pre-dispatch lane
// reservation in runQuery, which refuses honestly because nothing has run yet.
// Once the rows exist, honesty is the only remedy left.
func (l *Listener) reportOutputWithheld(conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, peer string, closeReason *string, why outputWithheld,
	stopped *exec.EmitStopped, targetFailed bool, ownsTerminal bool) bool {

	reason, known := withheldReasons[why]
	if !known {
		// A stop reason with no row in the table is a front-door defect, and
		// guessing a message for it would put invented prose on the wire. Say
		// what is true — the outcome is unknown — and stop.
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleUnframeableMessage, Peer: peer,
			Detail: fmt.Sprintf("unnamed output-withheld reason %d", int(why))})
		*closeReason = "unframeable-message"
		return false
	}

	// ONE SNAPSHOT FOR BOTH THE STORY AND THE READINESS BYTE (r0 MF1).
	//
	// The engine's report is taken WHILE it holds the session's claim; a
	// WireTxStatus read afterwards is a second, later snapshot, and the claim
	// has been released in between. Deriving the effects clause from the first
	// and the readiness byte from the second is how a client is told its effects
	// are PENDING and then handed readiness `I` in the same cycle — the two
	// halves of one answer disagreeing, which is the defect this whole path
	// exists to prevent (r5 MF16) reappearing between the arm and the byte.
	//
	// So when the engine reported, its TxStatus is authoritative for BOTH. The
	// separate read remains only for the paths that have no report — the
	// extended path today — where there is one snapshot anyway.
	// A REPORT THAT EXISTS IS THE ONLY SNAPSHOT, VALID OR NOT (r1 residual 1).
	//
	// The first repair asked `stopped != nil && validTxStatus(...)`, which reads
	// an INVALID status on a real report as though there were no report at all
	// and repairs it from the later snapshot — reintroducing the split it was
	// fixing, in the one case where the engine's answer is already suspect. If
	// the engine reported and its status is not a status, the session's phase is
	// unknown and §6.3's rule applies: no readiness is invented for it.
	status := byte(0)
	switch {
	case stopped != nil:
		if !validTxStatus(stopped.TxStatus) {
			*closeReason = "session-lost"
			return false
		}
		status = stopped.TxStatus
	default:
		var serr error
		status, serr = l.queries.WireTxStatus(sess.SessionID, sess.UserID)
		if serr != nil || !validTxStatus(status) {
			*closeReason = "session-lost"
			return false
		}
	}
	lead, effects, outcome := recordedEffects(stopped, status, targetFailed)

	l.onEvent(Event{Kind: "fd.stmt_outcome", Reason: reason.rule, Peer: peer,
		Detail: fmt.Sprintf("effects=%s; output withheld: %s", outcome, reason.stopped)})
	be.Send(gateError("ERROR", sqlStateProgramLimit,
		lead+"; "+reason.stopped+" and its result was not fully delivered",
		reason.rule, effects+"; "+reason.remedy))
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}

	// THE TERMINAL BYTE BELONGS TO THE CYCLE THAT OWNS IT (lector r2 MF4).
	//
	// A simple Query's cycle ends here: the session is intact — a budget stopped
	// the OUTPUT, not the connection — so §6.3 owes it the readiness that ends
	// every surviving cycle, and this is the only place it can come from.
	//
	// AN EXTENDED SEGMENT DOES NOT END HERE. Only the client's Sync ends one, and
	// Sync's ReadyForQuery is that segment's terminal. Sending one here puts TWO
	// on the wire for a single segment, and a client reads the second as the
	// answer to whatever it sends NEXT — so a following SELECT consumes a stale
	// readiness and returns someone else's frames. That is stream
	// desynchronisation, not a spare frame, and it is invisible to any cell that
	// stops reading at the first readiness it sees.
	if !ownsTerminal {
		return true
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// recordedEffects says what became of a statement whose output was cut short.
//
// IT SWITCHES ON THE ENGINE'S ANSWER AND DOES NOT RE-DERIVE ONE. exec.EmitStopped
// carries the arm; asking it twice — once in the engine, once from the fields
// here — is how the two stories drift, and this path exists because the client's
// story and the audit's disagreed once already (r5 MF16).
//
// The arm ORDER is the engine's too, and it is load-bearing: lector's r1 on #60
// found that placing the empty-query arm after the transaction arms would tell a
// client its NON-EXISTENT statement's effects were "pending" inside a BEGIN.
// Order is code there, not documentation, which is why this switch has no
// ordering of its own to get wrong.
//
// NOT EVERY PATH HAS AN ENGINE REPORT. exec.EmitStopped is produced by
// WireQuery and, since the extended truth fix, by WireExecutePortal — but NOT by
// WireFlushSegment, which still returns its drain's bare emitFailure. So a stop
// during an Execute arrives armed and a stop during a standalone Flush does not,
// and the fallback below is still reached by the Flush drive. Swapping the
// emitter's own observation out for the engine's would therefore make that path
// REPORT LESS than it does today — "unresolved" where it can currently say
// "failed" — which is a regression dressed as a refactor.
//
// So both are kept, engine first: the engine's answer when there is one, the
// emitter's observation when there is not. The observation is not a guess — the
// emitter holds the target's message before deciding whether to forward it — it
// is simply narrower, because a failure arriving after the stop is invisible to
// it. The second branch goes when Flush and Sync arm their stops too; until
// then, deleting it would silently downgrade a Flush cut to "unresolved".
//
// The fallback arm is deliberate rather than defensive: an unrecognised arm is
// something the engine grew and the front door has not been taught, and saying
// "not known" is the only honest answer to a vocabulary this function does not
// speak — never a guess dressed as a fact.
func recordedEffects(stopped *exec.EmitStopped, status byte, targetFailed bool) (lead, clause, outcome string) {
	arm := armFromWhatIsKnown(stopped, status, targetFailed)
	switch arm {
	case exec.ArmNoStatement:
		// The empty query. Nothing ran, so there are no effects to speak about
		// at all — and saying anything about them would be inventing a statement
		// for the client to worry about.
		return "the query was empty, so nothing ran",
			"there are no effects; the empty query's own reply was not delivered", string(arm)
	case exec.ArmNotExecuted:
		// IT EXISTED AND IT DID NOT RUN. Not the empty query above — the client
		// sent this statement, and an earlier one in the same buffer or segment
		// failed, so the target discarded it. Saying "the query was empty" here
		// tells a client that sent real SQL that it sent nothing.
		return "an earlier statement in this batch failed, so this one did not run",
			"it has no effects; the earlier statement's failure discarded it", string(arm)
	case exec.ArmFailed:
		return "the statement failed at the target",
			"its effects were rolled back", string(arm)
	case exec.ArmPending:
		return "the statement executed",
			"the statement's effects are PENDING: the transaction is still open, so COMMIT or ROLLBACK still decides them",
			string(arm)
	case exec.ArmAborted:
		return "the statement executed",
			"the transaction is aborted, so the statement's effects will be rolled back", string(arm)
	case exec.ArmCompleted:
		// The engine watched it finish. This is the one arm the loop could never
		// reach on its own: stopping early is this path's premise, so "committed"
		// was not a conclusion available to it until the engine could report the
		// drained tail (r5 MF16, jarvis's seam).
		return "the statement executed",
			"the statement's effects are committed", string(arm)
	default:
		return "the statement ran and its outcome is not known to the front door",
			"the front door stopped reading before the target reported the outcome, so whether the effects were kept is unresolved — read the table to find out",
			string(exec.ArmUnresolved)
	}
}

// armFromWhatIsKnown prefers the ENGINE's answer and falls back to what the
// emitter saw, so a path without an engine report still says as much as it can
// establish rather than defaulting to "unknown".
func armFromWhatIsKnown(stopped *exec.EmitStopped, status byte, targetFailed bool) exec.EmitArm {
	if stopped != nil {
		return stopped.Arm()
	}
	switch {
	case targetFailed:
		// Seen passing through the emitter: certain, just narrower than the
		// engine's view.
		return exec.ArmFailed
	case status == txStatusInTx:
		return exec.ArmPending
	case status == txStatusAborted:
		return exec.ArmAborted
	default:
		// Idle with nothing observed is NOT "committed" — that was r5 MF16.
		return exec.ArmUnresolved
	}
}

// frameGateError turns the front door's OWN refusal into a §8a ErrorResponse and
// the readiness that follows it. A target error never reaches here — it arrives
// as protocol data through emit and WireQuery returns a status normally.
func (l *Listener) frameGateError(conn net.Conn, be *pgproto3.Backend, sess exec.WireSessionResult,
	err error, peer string, closeReason *string) bool {

	code, rule, hint, fatal := classifyGateError(err)
	l.onEvent(Event{Kind: "fd.refused", Reason: rule, Peer: peer, Detail: err.Error()})

	severity := "ERROR"
	if fatal {
		severity = "FATAL"
	}
	be.Send(gateError(severity, code, gateMessage(err), rule, hint))
	if fatal {
		_ = l.flushBounded(conn, be)
		*closeReason = rule
		return false
	}

	// The refusal did not end the session, so the client is owed a readiness
	// byte. It comes from the engine's state machine rather than being assumed
	// idle: a gate refusal INSIDE a transaction leaves that transaction open,
	// and telling the client "idle" would invite it to start another.
	return l.sendReadiness(conn, be, sess, peer, closeReason)
}

// applyDispatch emits a decision's frame, if it has one, and records its audit
// event. It does not close the connection — the caller owns the loop's exit, so
// that a decision cannot end a session the caller still believes is running.
func (l *Listener) applyDispatch(conn net.Conn, be *pgproto3.Backend, d dispatch, peer string, closeReason *string) {
	if d.auditKind != "" {
		l.onEvent(Event{Kind: d.auditKind, Reason: d.auditReason, Peer: peer, Detail: d.auditDetail})
	}
	if d.emit != nil {
		be.Send(d.emit)
		// Bounded like every other post-auth write: a peer that will not read
		// must not park the session on a refusal frame either (r2 MF13). A
		// failed flush is not reported — the decision is made and the audit row
		// is written; the only remaining question is whether the peer heard it,
		// which changes nothing this end.
		_ = l.flushBounded(conn, be)
	}
	if d.after == endSession && d.closeReason != "" {
		*closeReason = d.closeReason
	}
}

// errUnframeableKind is a backend message whose KIND the front door does not
// know, as distinct from one whose kind it knows and whose payload is malformed.
//
// The distinction is load-bearing for the vocabulary witness: probing "does
// backendFrame accept this kind" with an empty message cannot otherwise tell a
// missing case from a missing payload, and ErrorResponse legitimately refuses an
// empty one. It is also the more useful error for an operator — "I do not know
// this message" and "this message arrived broken" are different defects.
var errUnframeableKind = errors.New("unframeable backend message kind")

// errCanaryMessage is a §5 canary: a message whose ARRIVAL is itself the defect,
// because its trigger is refused before the target could produce one.
//
// Distinct from errUnframeableKind on purpose. A canary has a CASE — the front
// door knows exactly what it is and refuses it deliberately — while an
// unframeable kind has none. Sharing one error would make "we never thought
// about this" and "we decided this is impossible" indistinguishable, which is
// the confusion the vocabulary convention exists to prevent.
var errCanaryMessage = errors.New("backend message whose arrival is itself a defect")

// ruleClassifierBypass is the audit cause for a §5 canary arriving from the
// target.
//
// SEPARATE FROM ruleUnframeableMessage, and the separation is the point. A canary
// means something reached the target that CLASSIFICATION was supposed to refuse —
// a gate bypass. Auditing it as an unframeable message records a security event
// as our mapper being incomplete, and sends an operator to debug the front door
// while the thing that actually happened goes unnamed. The wire still gets the
// catalogue's violation id (§7 names one id for that class); the AUDIT gets the
// defect, which is the §1.2 split.
const ruleClassifierBypass = "frontdoor/classifier-bypass"

// unframeableAudit picks the audit identity for a message the front door would
// not forward: a deliberate canary refusal, or a kind it has no case for.
func unframeableAudit(err error) string {
	if errors.Is(err, errCanaryMessage) {
		return ruleClassifierBypass
	}
	return ruleUnframeableMessage
}

// unframeableMessageText is what the PEER is told, which differs for the same
// reason: "the front door cannot forward this" is false for a canary — the front
// door understood it exactly and refused it.
func unframeableMessageText(err error) string {
	if errors.Is(err, errCanaryMessage) {
		return "the server produced a message that cannot occur through this front door"
	}
	return "the server produced a message the front door cannot forward"
}

// unframeableHint is the remediation half of the same split.
func unframeableHint(err error) string {
	if errors.Is(err, errCanaryMessage) {
		return "this indicates a statement reached the target that classification refuses; the statement's outcome is unknown"
	}
	return "this is a front-door defect; the statement's outcome is unknown"
}

// ruleUnframeableMessage is the audit cause for a backend message the front door
// cannot turn into a frame. It is a front-door defect, so it is audited under
// its own cause even though the wire is told the catalogue's violation id.
const ruleUnframeableMessage = "frontdoor/unframeable-message"

// pendingOutputWatermark is matrix §8.4's bound on serialized output waiting to
// go out. Reaching it flushes, which is what pauses reads from the target: the
// loop cannot ask the engine for another message while it is writing this one.
const pendingOutputWatermark int64 = 4 << 20

// ruleOutputWatermark identifies the per-connection backpressure in the audit
// trail; ruleBudgetBackpressure identifies the process-wide lane's (§7).
const (
	ruleOutputWatermark    = "frontdoor/output-watermark"
	ruleBudgetBackpressure = "frontdoor/budget-backpressure"
)

// ruleSessionDeadline is §7's identity for the front door closing an idle
// session, and sqlStateIdleSessionTimeout is the SQLSTATE PostgreSQL itself uses
// for it — a client that already recognises 57P05 from a real server recognises
// it here.
const (
	ruleSessionDeadline        = "gate/session-deadline"
	sqlStateIdleSessionTimeout = "57P05"

	// §7's partial-frame progress budget identity.
	ruleFrameStall            = "frontdoor/frame-stall"
	sqlStateConnectionFailure = "08006"

	// §7's cumulative-output cap identity.
	ruleOutputCap        = "frontdoor/output-cap"
	sqlStateProgramLimit = "54000"
	// cumulativeOutputCap is §7's per-statement bound on total output.
	cumulativeOutputCap int64 = 8 << 30
)

// deadlineGoodbyeBudget bounds the write of the frame that explains a deadline
// expiry. Short: the peer is already past its budget and this is a courtesy, not
// a reason to hold the slot open.
const deadlineGoodbyeBudget = 2 * time.Second

// classifyGateError maps a front-door refusal onto the §7 refusal catalogue:
// SQLSTATE, the DETAIL rule id, the HINT, and whether the connection survives.
//
// Refusals the catalogue does not name fall through to a denial-shaped answer
// rather than an invented code. Guessing a SQLSTATE would be worse than a
// generic one: a client branches on the code, and a wrong branch is a wrong
// recovery.
func classifyGateError(err error) (code, rule, hint string, fatal bool) {
	switch {
	case errors.Is(err, exec.ErrWireFaceLost):
		// The session's wire failed and the engine is already closing the
		// session. There is nothing left to be ready for, so the connection is
		// torn down with NO readiness byte — asserting a transaction state over
		// a wire that has gone would be a claim we cannot support.
		//
		// Matched EXPLICITLY rather than inferred from a later WireTxStatus
		// failure. The outcome would be the same today, but correctness that
		// depends on a downstream call happening to fail is correctness that
		// breaks silently when that call changes.
		return sqlStateProtocolViolation, "frontdoor/wire-face-lost",
			"the session's connection to the target failed; reconnect", true

	case errors.Is(err, exec.ErrDecodedResultTruncated):
		// Only a NON-PostgreSQL target can reach this now: PostgreSQL streams
		// unbounded through the raw path. The decoded producer REFUSES rather
		// than dropping rows, which is what §5 requires of anything that cannot
		// serve a result whole.
		return "54000", exec.DecodedResultTruncatedRuleID,
			"the result exceeds the decoded producer's page; this target does not stream unbounded results", false

	case errors.Is(err, exec.ErrMultiStatement):
		return sqlStateFeatureNotSupported, "gate/no-multi-statement",
			"send one statement per Query message", false

	case errors.Is(err, exec.ErrScriptTooLarge):
		return "54000", "frontdoor/statement-too-large",
			"reduce the statement size", false

	case errors.Is(err, auth.ErrDenied):
		return "42501", "gate/insufficient-privilege",
			"the credential does not carry the privilege this statement needs", false

	case errors.Is(err, exec.ErrSessionNotFound):
		// The session is gone; there is nothing left to be ready for.
		return sqlStateProtocolViolation, "frontdoor/session-lost",
			"the session ended; reconnect", true
	}
	return "42501", "gate/refused",
		"the front door refused this statement", false
}

// gateMessage is what the peer is TOLD a refusal was. It is deliberately the
// error's own text for refusals the engine authored, because those were written
// for a caller to read; it never includes internal identifiers, which travel in
// the audit row instead.
func gateMessage(err error) string {
	return err.Error()
}

// estimateFrameBytes approximates a message's serialized size for the watermark.
//
// An estimate, deliberately: pgproto3 gives no way to ask the Backend how much
// it is holding, and the alternative — encoding twice to measure — would double
// the work on the hot path. The payload dominates the framing overhead for the
// messages that can actually grow a buffer (DataRow, RowDescription), so an
// estimate that counts payload and ignores a few bytes of header is wrong by a
// margin that does not matter to a 4 MiB bound.
func estimateFrameBytes(m exec.WireMessage) int {
	n := 8 // type byte + length, plus slack for small fixed fields
	for _, v := range m.Values {
		n += len(v) + 4 // each column carries its own length prefix
	}
	for _, f := range m.Fields {
		n += len(f.Name) + 18
	}
	n += len(m.Tag) + len(m.ParameterName) + len(m.ParameterValue)
	// Notices and target errors carry their whole payload in these fields, and
	// ignoring them meant a burst of large notices crossed no watermark at all
	// while filling the buffer — the exact case a >5 MiB Notice burst exposed.
	n += pgErrorBytes(m.Err) + pgErrorBytes((*pgconn.PgError)(m.Notice))
	if m.Notification != nil {
		n += len(m.Notification.Channel) + len(m.Notification.Payload) + 8
	}
	return n
}

// pgErrorBytes sizes an error/notice payload. Every field travels on the wire,
// so every field counts toward the buffer it fills.
func pgErrorBytes(e *pgconn.PgError) int {
	if e == nil {
		return 0
	}
	return len(e.Severity) + len(e.SeverityUnlocalized) + len(e.Code) + len(e.Message) +
		len(e.Detail) + len(e.Hint) + len(e.InternalQuery) + len(e.Where) +
		len(e.SchemaName) + len(e.TableName) + len(e.ColumnName) + len(e.DataTypeName) +
		len(e.ConstraintName) + len(e.File) + len(e.Routine) + 24
}

// backendFrame turns one neutral engine message into the wire frame that
// carries it. A kind this function does not know is an ERROR rather than a
// skip: §5's canaries (CopyInResponse, NotificationResponse, FunctionCallResponse
// and the rest) are messages whose ARRIVAL is itself the defect, and skipping
// one would hide exactly the event the canary exists to catch.
func backendFrame(m exec.WireMessage) (pgproto3.BackendMessage, error) {
	switch m.Kind {
	case "RowDescription":
		fields := make([]pgproto3.FieldDescription, len(m.Fields))
		for i, f := range m.Fields {
			fields[i] = pgproto3.FieldDescription{
				Name:                 []byte(f.Name),
				TableOID:             f.TableOID,
				TableAttributeNumber: f.ColumnAttr,
				DataTypeOID:          f.TypeOID,
				DataTypeSize:         f.TypeSize,
				TypeModifier:         f.TypeModifier,
				Format:               f.Format,
			}
		}
		return &pgproto3.RowDescription{Fields: fields}, nil

	case "DataRow":
		return &pgproto3.DataRow{Values: m.Values}, nil

	case "CommandComplete":
		return &pgproto3.CommandComplete{CommandTag: []byte(m.Tag)}, nil

	case "EmptyQueryResponse":
		return &pgproto3.EmptyQueryResponse{}, nil

	case "ErrorResponse":
		// The TARGET's error, forwarded with its own fields. The front door
		// never rewrites one: the client's own tooling reads these, and a
		// helpfully-reworded constraint violation is a lie about which
		// constraint fired.
		if m.Err == nil {
			return nil, errors.New("ErrorResponse with no error")
		}
		return targetErrorFrame(m.Err), nil

	case "NoticeResponse":
		if m.Notice == nil {
			return nil, errors.New("NoticeResponse with no notice")
		}
		return targetNoticeFrame(m.Notice), nil

	case "ParameterStatus":
		return &pgproto3.ParameterStatus{Name: m.ParameterName, Value: m.ParameterValue}, nil

	// THE EXTENDED PROTOCOL'S OWN REPLIES (F2). Absent until F2 because the
	// simple path cannot produce one: there is no Parse to complete and no
	// portal to suspend. Each carries no payload of its own beyond what the
	// frame type already says, except ParameterDescription, whose OIDs are the
	// SERVER's answer to a Describe and are forwarded like any other target
	// value.
	case "ParseComplete":
		return &pgproto3.ParseComplete{}, nil

	case "BindComplete":
		return &pgproto3.BindComplete{}, nil

	case "CloseComplete":
		return &pgproto3.CloseComplete{}, nil

	case "NoData":
		return &pgproto3.NoData{}, nil

	case "PortalSuspended":
		return &pgproto3.PortalSuspended{}, nil

	case "ParameterDescription":
		return &pgproto3.ParameterDescription{ParameterOIDs: m.ParameterOIDs}, nil

	// §5'S CANARIES, each with its own case rather than the default arm.
	//
	// Their triggers are all refused before the target could produce one, so an
	// arrival is itself the defect — and saying that in code is the difference
	// between "this is impossible, here is why" and "we never thought about
	// this". Six of these used to fall into the default, which said the latter
	// while the matrix said the former; the vocabulary witness is what surfaced
	// the disagreement.
	case "CopyInResponse", "CopyOutResponse", "CopyBothResponse":
		return nil, fmt.Errorf("%w: %s, but COPY and replication are refused at classification", errCanaryMessage, m.Kind)

	case "CopyData", "CopyDone":
		return nil, fmt.Errorf("%w: backend-direction %s, but no COPY sub-protocol is ever active", errCanaryMessage, m.Kind)

	case "FunctionCallResponse":
		return nil, fmt.Errorf("%w: FunctionCallResponse, but the fast-path is refused (row 4:FunctionCall)", errCanaryMessage)

	case "NotificationResponse":
		// A canary: LISTEN is refused at classification, so a notification can
		// only mean the classifier was bypassed.
		return nil, fmt.Errorf("%w: NotificationResponse, which LISTEN's refusal makes impossible", errCanaryMessage)
	}
	return nil, fmt.Errorf("%w: %q", errUnframeableKind, m.Kind)
}

// outputAccountant is the ONE place output is accounted for on its way to the
// client: §7's cumulative cap, §8.4's watermark, the general lane's top-up, and
// the borrowed-bytes contract.
//
// It is a type rather than a closure because F2's extended segments stream
// through it too. A second copy of this logic is the failure mode the budgets
// document exists to prevent — a stream that bypasses the accounting bypasses
// every bound at once, and nothing observes it until a process runs out of
// memory.
type outputAccountant struct {
	l    *Listener
	conn net.Conn
	be   *pgproto3.Backend
	peer string

	// held is the working set reserved from the general lane. It grows only for
	// a single frame larger than the whole reservation, and the owner's release
	// reads it back, so a top-up cannot leak.
	held int64
	// pending is what sits in the Backend's write buffer; produced is the
	// cumulative output of the statement or segment.
	pending, produced int64

	withheld     outputWithheld
	targetFailed bool
	emitErr      error
	writeErr     error
}

func newOutputAccountant(l *Listener, conn net.Conn, be *pgproto3.Backend, peer string, held int64) *outputAccountant {
	return &outputAccountant{l: l, conn: conn, be: be, peer: peer, held: held, withheld: outputComplete}
}

// emit accounts for one engine message and forwards it.
func (a *outputAccountant) emit(m exec.WireMessage) error {
	l, conn, be, peer := a.l, a.conn, a.be, a.peer
	if m.Err != nil {
		a.targetFailed = true
	}
	frame, ferr := backendFrame(m)
	if ferr != nil {
		a.emitErr = ferr
		return ferr
	}
	if frame != nil {
		// ACCOUNTED BEFORE SERIALIZATION (§8.4: "before serialization of
		// each outbound frame"). Counting after Send means the buffer has
		// already grown by the frame that crossed the line, so the
		// watermark is enforced one frame late — and a single frame can
		// be large.
		size := estimateFrameBytes(m)

		// §7's cumulative per-statement output cap. Unlike the watermark,
		// which paces, this ABORTS: past it the statement is refused
		// rather than allowed to stream forever.
		a.produced += int64(size)
		if a.produced > l.outputCap() {
			a.withheld = withheldAtCap
			return errStopForwarding
		}

		// THE WATERMARK IS ENFORCED BEFORE SERIALIZATION (§8.4: "before
		// serialization of each outbound frame"), which means draining
		// FIRST when this frame would cross it.
		//
		// Checking after Send would let the buffer grow by the frame that
		// crossed the mark before anything drained it — the allocation
		// the bound exists to prevent has already happened by the time
		// the bound notices. One frame can be large.
		// The session's working set is ALREADY RESERVED above, so an
		// ordinary frame touches the lane not at all — it draws against
		// what this statement holds. Only a frame bigger than the whole
		// reservation needs more, and it asks for it after flushing,
		// because a flush empties the buffer and may make the extra
		// unnecessary.
		if int64(size) > a.held {
			l.onEvent(Event{Kind: "fd.backpressure_enter", Reason: ruleBudgetBackpressure, Peer: peer})
			if ferr := l.flushBounded(conn, be); ferr != nil {
				a.writeErr = ferr
				return ferr
			}
			a.pending = 0
			ok := l.general.reserve(int64(size)-a.held, l.laneWait(), l.now)
			l.onEvent(Event{Kind: "fd.backpressure_exit", Reason: ruleBudgetBackpressure, Peer: peer})
			if !ok {
				// The statement HAS RUN. See reportOutputWithheld.
				a.withheld = withheldOnSaturatedLane
				return errStopForwarding
			}
			a.held = int64(size)
		}

		if a.pending+int64(size) >= l.outputWatermark() && a.pending > 0 {
			l.onEvent(Event{Kind: "fd.backpressure_enter", Reason: ruleOutputWatermark, Peer: peer})
			if ferr := l.flushBounded(conn, be); ferr != nil {
				// A failed write is the SOCKET, not our framing. Keeping
				// one error for both made a client hanging up mid-result
				// audit as a front-door defect and told the peer the
				// SERVER had produced something unforwardable.
				a.writeErr = ferr
				return ferr
			}
			l.onEvent(Event{Kind: "fd.backpressure_exit", Reason: ruleOutputWatermark, Peer: peer})
			a.pending = 0
		}
		a.pending += int64(size)

		// Send ENCODES here, synchronously, appending to the backend's
		// write buffer — it does not retain the message. That is what
		// makes it safe to hand it a DataRow whose Values are BORROWED
		// for the duration of this call: the bytes are copied out before
		// emit returns.
		//
		// A refactor that queued frames to send after the callback
		// returned would read those values after the producer had reused
		// the memory, and the corruption would be silent and
		// data-dependent. Anything deferred must be copied first.
		be.Send(frame)
	}
	return nil
}
