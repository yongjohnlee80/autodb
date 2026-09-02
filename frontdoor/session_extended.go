package frontdoor

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE EXTENDED-QUERY ROUTING (F2's wire half, matrix §4).
//
// These seven frames are ROUTED, not decided. They used to be one arm of
// dispatchFrame returning a FATAL 0A000; that arm is gone, and the reason it
// could not simply become a different table entry is in the table's own type:
// `dispatch.emit` is ONE *pgproto3.ErrorResponse, while Execute and Flush
// produce STREAMS. Widening that field to carry a stream would dissolve the
// property the table exists for — that every decision in it is testable without
// a socket, an engine or a target.
//
// It is the same reason Query is absent from the table: not one of these frames
// is decidable from the frame alone. Parse needs the classifier and the gate,
// Execute needs a fresh authority resolution, Sync's readiness byte comes from
// the engine's state machine, and object lifetimes are §4a rules that span many
// frames. So the loop forwards and frames; the engine decides.
//
// Johno's "one FATAL 0A000, then close" ruling for these frames is superseded by
// F2 LANDING, not by this file deciding it is: the ruling was explicitly for the
// window before F2 existed.

// extendedFrame reports whether msg is one of the extended-query frames this
// file routes.
func extendedFrame(msg pgproto3.FrontendMessage) bool {
	switch msg.(type) {
	case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe,
		*pgproto3.Execute, *pgproto3.Close, *pgproto3.Flush, *pgproto3.Sync:
		return true
	}
	return false
}

// runExtended routes one extended frame to the engine and frames what comes
// back. It reports whether the session continues.
//
// seg carries the SEGMENT's general-lane reservation across frames, because a
// segment spans many of them and the working set exists for the whole of it —
// see reserveSegment for why it cannot be per frame.
func (l *Listener) runExtended(ctx context.Context, conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, msg pgproto3.FrontendMessage, peer string,
	seg *segmentLane, closeReason *string) bool {

	// DISCARD-THROUGH-SYNC. Only Sync ends it; Terminate is handled by the outer
	// decision table, which never reaches here.
	if seg.discarding {
		if _, isSync := msg.(*pgproto3.Sync); !isSync {
			return true
		}
	}

	id, uid := sess.SessionID, sess.UserID
	var err error

	switch m := msg.(type) {
	case *pgproto3.Parse:
		err = l.queries.WireParse(ctx, id, uid, m.Name, m.Query, m.ParameterOIDs, hostOf(peer))

	case *pgproto3.Bind:
		err = l.queries.WireBind(ctx, id, uid, m.DestinationPortal, m.PreparedStatement,
			m.Parameters, m.ParameterFormatCodes, m.ResultFormatCodes)

	case *pgproto3.Describe:
		if m.ObjectType == 'P' {
			err = l.queries.WireDescribePortal(ctx, id, uid, m.Name)
		} else {
			err = l.queries.WireDescribeStatement(ctx, id, uid, m.Name)
		}

	case *pgproto3.Close:
		if m.ObjectType == 'P' {
			err = l.queries.WireClosePortal(ctx, id, uid, m.Name)
		} else {
			err = l.queries.WireCloseStatement(ctx, id, uid, m.Name)
		}

	case *pgproto3.Execute:
		return l.runExtendedStream(ctx, conn, be, sess, peer, seg, closeReason,
			func(emit func(exec.WireMessage) error) error {
				return l.queries.WireExecutePortal(ctx, id, uid, m.Portal, m.MaxRows, hostOf(peer), emit)
			})

	case *pgproto3.Flush:
		return l.runExtendedStream(ctx, conn, be, sess, peer, seg, closeReason,
			func(emit func(exec.WireMessage) error) error {
				return l.queries.WireFlushSegment(ctx, id, uid, emit)
			})

	case *pgproto3.Sync:
		return l.runExtendedSync(conn, be, sess, peer, seg, closeReason)
	}

	if err != nil {
		return l.frameExtendedError(conn, be, sess, err, peer, seg, closeReason)
	}
	// Parse, Bind, Describe and Close produce no frame of their own here: their
	// answers are queued in the engine's segment and delivered when the client
	// asks for them with Flush or Sync, exactly as PostgreSQL delivers them. A
	// front door that answered each one immediately would send bytes earlier
	// than the server it is standing in for.
	return true
}

// runExtendedSync ends the segment and sends the readiness byte the engine's own
// state machine reports.
//
// Sync is also the only thing that ends a post-error discard (matrix row
// 4:discard): after a server ErrorResponse the target ignores every frame but
// Sync and Terminate, and the engine's Sync is what consumes through to the
// terminal ReadyForQuery. The loop must never synthesise this byte.
func (l *Listener) runExtendedSync(conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, peer string, seg *segmentLane, closeReason *string) bool {

	status, err := l.queries.WireSyncSegment(context.Background(), sess.SessionID, sess.UserID)
	// The segment is over either way: on success the target answered or discarded
	// everything, and on failure the wire is unusable. Release before deciding
	// what to tell the client, so no exit path can skip it.
	seg.release(l)
	seg.discarding = false
	if err != nil {
		return l.frameExtendedError(conn, be, sess, err, peer, seg, closeReason)
	}
	if !validTxStatus(status) {
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleUnframeableMessage, Peer: peer,
			Detail: "extended Sync reported an invalid transaction status"})
		*closeReason = "invalid-tx-status"
		return false
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// frameExtendedError turns an engine refusal into a §8a ErrorResponse.
//
// It does NOT send readiness. In the extended protocol the client's own Sync is
// what asks for it, and PostgreSQL answers an error mid-segment by discarding
// until that Sync — so synthesising readiness here would end a cycle the client
// has not ended, and the next frames the client already pipelined would arrive
// against a server that thinks the segment is over.
func (l *Listener) frameExtendedError(conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, err error, peer string, seg *segmentLane, closeReason *string) bool {

	code, rule, hint, fatal := classifyGateError(err)
	l.onEvent(Event{Kind: "fd.refused", Reason: rule, Peer: peer, Detail: err.Error()})

	severity := "ERROR"
	if fatal {
		severity = "FATAL"
	}
	be.Send(gateError(severity, code, gateMessage(err), rule, hint))
	if ferr := l.flushBounded(conn, be); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	if fatal {
		*closeReason = rule
		return false
	}
	// The segment is now discarding: this refusal is OURS, so the target never
	// saw it and will not ignore what follows. We must.
	seg.discarding = true
	return true
}

// segmentLane is the general-lane reservation held for the lifetime of ONE
// extended segment.
//
// It is segment-scoped rather than per frame, and the reason is the protocol:
// PostgreSQL does not send until Flush or Sync, so the answers to an Execute sit
// buffered until the client asks for them. A reservation released when Execute
// returned would be taken while the working set is small and released while it
// is largest — backwards, not merely smaller.
//
// §8.2 is release on EVERY path, and Sync is only the segment's NORMAL exit. A
// client that vanishes between Execute and Sync never sends one, so the owning
// loop holds this in a defer for the session's lifetime: teardown, disconnect
// and every abandonment release it too. A release-at-Sync design would leak
// those bytes for the life of the process.
type segmentLane struct {
	held int64

	// discarding is the front door's OWN ignore_till_sync.
	//
	// PostgreSQL discards every frame but Sync and Terminate after an error in a
	// segment — but the target only starts that when IT produced the error. A
	// Parse refused at our gate, or an Execute refused on re-authorization, never
	// reaches the target, so nothing there starts it and the frames the client
	// already pipelined behind the failure would be executed as though the
	// refusal had not happened. This is the same rule, for the errors we raise.
	//
	// It lives on the segment, beside the reservation, because both are segment
	// state the loop owns and Sync ends both.
	discarding bool
}

// reserveSegment takes the segment's output working set before its FIRST frame
// reaches the engine — the segment's own "before dispatch", so that ordinary
// saturation is refused while refusing is still TRUE: nothing has executed and
// nothing is durable.
func (l *Listener) reserveSegment(conn net.Conn, be *pgproto3.Backend,
	seg *segmentLane, peer string, closeReason *string) bool {

	if seg.held > 0 {
		return true // already reserved for this segment
	}
	held := l.outputWatermark()
	if capacity := l.general.capacity(); held > capacity {
		held = capacity
	}
	if !l.general.reserve(held, l.laneWait(), l.now) {
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleBudgetBackpressure, Peer: peer,
			Detail: "the general lane could not admit this segment's output working set; nothing was dispatched"})
		be.Send(gateError("ERROR", sqlStateProgramLimit,
			"the server is at its output budget and did not run this statement", ruleBudgetBackpressure,
			"nothing was executed; retry when the server is less busy"))
		if ferr := l.flushBounded(conn, be); ferr != nil {
			*closeReason = "write-failed"
		}
		return false
	}
	seg.held = held
	return true
}

// release returns whatever the segment holds. Idempotent, because the owning
// defer runs on paths where Sync has already released.
func (s *segmentLane) release(l *Listener) {
	if s.held > 0 {
		l.general.release(s.held)
		s.held = 0
	}
}

// runExtendedStream drives one output-producing frame through the SAME
// accounting every other stream uses.
//
// The accountant is shared with runQuery deliberately: a stream that carries its
// own copy of the watermark, the cumulative cap and the lane top-up bypasses
// every bound at once, and nothing observes it until the process runs out of
// memory.
func (l *Listener) runExtendedStream(ctx context.Context, conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, peer string, seg *segmentLane, closeReason *string,
	drive func(emit func(exec.WireMessage) error) error) bool {

	// RESERVED HERE, at the first frame of the segment that can PRODUCE output —
	// the segment's own "before dispatch", so ordinary saturation is refused
	// while refusing is still TRUE: nothing has executed and nothing is durable.
	//
	// Not at the segment's first frame (jarvis, 2026-09-03): a segment that has
	// only Parsed and Bound has produced nothing, holds no working set, and is a
	// client merely between messages. Charging it would put a second timer on an
	// idle session and hold lane bytes for output that does not exist.
	if !l.reserveSegment(conn, be, seg, peer, closeReason) {
		return false
	}

	acct := newOutputAccountant(l, conn, be, peer, seg.held)
	// The top-up belongs to the SEGMENT, so it travels back onto it rather than
	// being released when this frame returns — the next Execute in the same
	// segment draws against the same working set.
	defer func() { seg.held = acct.held }()

	err := drive(acct.emit)

	switch {
	case acct.withheld != outputComplete:
		return l.reportOutputWithheld(conn, be, sess, peer, closeReason, acct.withheld, acct.targetFailed)

	case acct.writeErr != nil:
		l.onEvent(Event{Kind: "fd.conn_close", Reason: "write-failed", Peer: peer, Detail: acct.writeErr.Error()})
		*closeReason = "write-failed"
		return false

	case acct.emitErr != nil:
		l.onEvent(Event{Kind: "fd.refused", Reason: unframeableAudit(acct.emitErr), Peer: peer, Detail: acct.emitErr.Error()})
		be.Send(gateError("FATAL", sqlStateProtocolViolation,
			unframeableMessageText(acct.emitErr), ruleProtocolViolation,
			unframeableHint(acct.emitErr)))
		_ = l.flushBounded(conn, be)
		*closeReason = "unframeable-message"
		return false

	case err != nil:
		return l.frameExtendedError(conn, be, sess, err, peer, seg, closeReason)
	}
	// NOT FLUSHED HERE. The client asks for delivery with Flush or Sync, exactly
	// as it would of PostgreSQL; flushing now would send bytes earlier than the
	// server this stands in for, and the fidelity requirement is explicit.
	return true
}

// ruleSegmentStall is the identity for a client that opened an extended segment,
// holds pending output, and has neither Synced nor Flushed within
// segmentStallBudget (jarvis's ruling, 2026-09-03).
//
// It is frame-stall's SIBLING one level up — a whole segment left half-open
// rather than one frame — so it shares 08006's class. It is deliberately NOT
// 57P05 gate/session-deadline: that says the session was idle, and this client
// was not idle, it asked for output and did not collect it. And it is not
// write-failed: the peer may well be reading, it simply never ended its segment.
const ruleSegmentStall = "frontdoor/segment-stall"

// segmentStallBudget bounds a segment that holds a reservation and is waiting
// for the client's next frame.
//
// ITS OWN CONSTANT, deliberately. It is 30s, and so are idle-in-lane waiting and
// the output stall, but three budgets that happen to share a number are three
// MEANINGS (budgets doc §5.1): idle measures a client that is not asking,
// outputStall measures one that will not take what it is being sent, and this
// measures one that asked for output and then left the segment open. Sharing a
// constant would make a later change to one silently change the others.
const segmentStallBudget = 30 * time.Second

// segmentStall is the effective segment budget, so a cell can shorten it and
// still exercise the real enforcement path.
func (l *Listener) segmentStall() time.Duration {
	if l.testSegmentStall != nil {
		return *l.testSegmentStall
	}
	return segmentStallBudget
}

// segmentDeadline reports the budget for the wait BEFORE the next frame.
//
// A segment holding a reservation has pending output and a client that owes us a
// Sync, so it gets the stall budget. A segment that has only Parsed and Bound
// holds nothing and is a client merely between messages, so it stays under the
// idle clock — jarvis's narrowing, and the reason this asks the reservation
// rather than "is a segment open".
func (l *Listener) segmentDeadline(seg *segmentLane) time.Duration {
	if seg.held > 0 {
		return l.segmentStall()
	}
	return l.dl.idle
}

// endOfSegmentWait is endOfIdleWait's sibling for a stalled segment.
func (l *Listener) endOfSegmentWait(conn net.Conn, be *pgproto3.Backend, err error,
	peer string, closeReason *string) error {

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		// The expired deadline bounds writes too, so the goodbye frame needs its
		// own budget — the same reason the idle path gives one.
		_ = conn.SetDeadline(l.now().Add(deadlineGoodbyeBudget))
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleSegmentStall, Peer: peer})
		be.Send(gateError("FATAL", sqlStateConnectionFailure,
			"terminating connection: an extended-query segment was left open", ruleSegmentStall,
			"send Sync to end the segment; the front door does not hold output for a segment indefinitely"))
		if ferr := l.flushBounded(conn, be); ferr != nil {
			// A peer that will not take the goodbye frame is a dead reader, and
			// that is what the record should say.
			*closeReason = "write-failed"
			return nil
		}
		*closeReason = ruleSegmentStall
		return nil
	}
	*closeReason = "peer-closed"
	return nil
}
