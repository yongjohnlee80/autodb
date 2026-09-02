package frontdoor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

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
// br is the SAME buffered reader the Backend decodes from, which is what makes
// the type-byte guard possible: peeking it here consumes nothing, so pgproto3
// still sees a complete stream. Handing the loop a different reader would
// silently split the stream in two and lose whichever bytes the other one held.
func (l *Listener) runSession(ctx context.Context, conn net.Conn, br *bufio.Reader,
	be *pgproto3.Backend, sess exec.WireSessionResult, peer string, closeReason *string) error {

	for {
		// THE IDLE BUDGET IS RE-ARMED PER MESSAGE (matrix §8.4: "refreshed on
		// message/state transitions"). net.Conn deadlines are ABSOLUTE, so a
		// single arming at session open would make the idle budget a cap on
		// session LIFETIME — a pooled connection doing steady work would die
		// mid-statement at thirty minutes, having never once been idle.
		if err := conn.SetDeadline(l.now().Add(l.dl.idle)); err != nil {
			*closeReason = "deadline"
			return nil
		}

		// The type byte, BEFORE pgproto3 decodes it. An undefined byte cannot be
		// told from a transport failure once Receive has turned it into an
		// unstructured error, and it must not be: one closes the connection with
		// an accurate 08P01 and the other closes it with nothing to say.
		head, err := br.Peek(1)
		if err != nil {
			return l.endOfRead(conn, be, err, peer, closeReason)
		}
		if !validFrontendType(head[0]) {
			d := unknownMessageType()
			l.applyDispatch(be, d, peer, closeReason)
			return nil
		}

		msg, err := be.Receive()
		if err != nil {
			return l.endOfRead(conn, be, err, peer, closeReason)
		}

		if d, decided := dispatchFrame(msg); decided {
			l.applyDispatch(be, d, peer, closeReason)
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
			if !l.sendReadiness(be, sess, peer, closeReason) {
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
			l.applyDispatch(be, d, peer, closeReason)
			return nil
		}
		if !l.runQuery(ctx, be, sess, q.String, peer, closeReason) {
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
func (l *Listener) endOfRead(conn net.Conn, be *pgproto3.Backend, err error, peer string, closeReason *string) error {
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
		_ = be.Flush()
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
func (l *Listener) sendReadiness(be *pgproto3.Backend, sess exec.WireSessionResult,
	peer string, closeReason *string) bool {

	status, err := l.queries.WireTxStatus(sess.SessionID, sess.UserID)
	if err != nil || !validTxStatus(status) {
		*closeReason = "session-lost"
		return false
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	if ferr := be.Flush(); ferr != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// runQuery executes one simple-query buffer and frames the response. It reports
// whether the session continues.
func (l *Listener) runQuery(ctx context.Context, be *pgproto3.Backend,
	sess exec.WireSessionResult, sql, peer string, closeReason *string) bool {

	// The emitter FRAMES AND RETURNS. It must not call back into the engine on
	// this session: WireQuery holds the session's one-in-flight claim across
	// every emit, so a re-entrant call gets ErrSessionBusy by design. Anything
	// this loop needs from the engine happens after WireQuery returns.
	var emitErr error
	pending := 0
	status, err := l.queries.WireQuery(ctx, sess.SessionID, sess.UserID, sql, hostOf(peer),
		func(m exec.WireMessage) error {
			frame, ferr := backendFrame(m)
			if ferr != nil {
				emitErr = ferr
				return ferr
			}
			if frame != nil {
				// Send ENCODES here, synchronously, appending to the backend's
				// write buffer — it does not retain the message. That is what
				// makes it safe to hand it a DataRow whose Values are BORROWED
				// for the duration of this call: the bytes are copied out
				// before emit returns.
				//
				// A refactor that queued frames to send after the callback
				// returned would read those values after the producer had
				// reused the memory, and the corruption would be silent and
				// data-dependent. If frames ever need to be deferred, they must
				// be copied first.
				be.Send(frame)

				// THE PENDING-OUTPUT WATERMARK (matrix §8.4). Send only appends
				// to the Backend's write buffer — it does not write — so a
				// per-row Send with one Flush at the end holds the WHOLE result
				// in memory. Against the raw path, which streams unbounded, that
				// is an OOM the size of whatever the client selected.
				//
				// Flushing bounds it. The watermark rather than a flush per row
				// because a syscall per row would cost more than it saves on a
				// wide result, and 4 MiB is the figure the matrix budgets.
				pending += estimateFrameBytes(m)
				if pending >= pendingOutputWatermark {
					l.onEvent(Event{Kind: "fd.backpressure_enter", Reason: ruleOutputWatermark, Peer: peer})
					if ferr := be.Flush(); ferr != nil {
						emitErr = ferr
						return ferr
					}
					l.onEvent(Event{Kind: "fd.backpressure_exit", Reason: ruleOutputWatermark, Peer: peer})
					pending = 0
				}
			}
			return nil
		})

	switch {
	case emitErr != nil:
		// A message the front door cannot frame is a defect on OUR side, not the
		// peer's — a target emitting something impossible (§5's never-emitted
		// canaries) lands here. Say so accurately and close; forwarding a guess
		// would be worse than stopping.
		l.onEvent(Event{Kind: "fd.refused", Reason: ruleUnframeableMessage, Peer: peer, Detail: emitErr.Error()})
		be.Send(gateError("FATAL", sqlStateProtocolViolation,
			"the server produced a message the front door cannot forward", ruleProtocolViolation,
			"this is a front-door defect; the statement's outcome is unknown"))
		_ = be.Flush()
		*closeReason = "unframeable-message"
		return false

	case err != nil:
		return l.frameGateError(be, sess, err, peer, closeReason)
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
	if err := be.Flush(); err != nil {
		*closeReason = "write-failed"
		return false
	}
	return true
}

// frameGateError turns the front door's OWN refusal into a §8a ErrorResponse and
// the readiness that follows it. A target error never reaches here — it arrives
// as protocol data through emit and WireQuery returns a status normally.
func (l *Listener) frameGateError(be *pgproto3.Backend, sess exec.WireSessionResult,
	err error, peer string, closeReason *string) bool {

	code, rule, hint, fatal := classifyGateError(err)
	l.onEvent(Event{Kind: "fd.refused", Reason: rule, Peer: peer, Detail: err.Error()})

	severity := "ERROR"
	if fatal {
		severity = "FATAL"
	}
	be.Send(gateError(severity, code, gateMessage(err), rule, hint))
	if fatal {
		_ = be.Flush()
		*closeReason = rule
		return false
	}

	// The refusal did not end the session, so the client is owed a readiness
	// byte. It comes from the engine's state machine rather than being assumed
	// idle: a gate refusal INSIDE a transaction leaves that transaction open,
	// and telling the client "idle" would invite it to start another.
	return l.sendReadiness(be, sess, peer, closeReason)
}

// applyDispatch emits a decision's frame, if it has one, and records its audit
// event. It does not close the connection — the caller owns the loop's exit, so
// that a decision cannot end a session the caller still believes is running.
func (l *Listener) applyDispatch(be *pgproto3.Backend, d dispatch, peer string, closeReason *string) {
	if d.auditKind != "" {
		l.onEvent(Event{Kind: d.auditKind, Reason: d.auditReason, Peer: peer})
	}
	if d.emit != nil {
		be.Send(d.emit)
		// A failed flush is not reported: the decision is already made, the
		// audit row is already written, and the only remaining question is
		// whether the peer heard it — which changes nothing this end.
		_ = be.Flush()
	}
	if d.after == endSession && d.closeReason != "" {
		*closeReason = d.closeReason
	}
}

// ruleUnframeableMessage is the audit cause for a backend message the front door
// cannot turn into a frame. It is a front-door defect, so it is audited under
// its own cause even though the wire is told the catalogue's violation id.
const ruleUnframeableMessage = "frontdoor/unframeable-message"

// pendingOutputWatermark is matrix §8.4's bound on serialized output waiting to
// go out. Reaching it flushes, which is what pauses reads from the target: the
// loop cannot ask the engine for another message while it is writing this one.
const pendingOutputWatermark = 4 << 20

// ruleOutputWatermark identifies the backpressure in the audit trail.
const ruleOutputWatermark = "frontdoor/output-watermark"

// ruleSessionDeadline is §7's identity for the front door closing an idle
// session, and sqlStateIdleSessionTimeout is the SQLSTATE PostgreSQL itself uses
// for it — a client that already recognises 57P05 from a real server recognises
// it here.
const (
	ruleSessionDeadline        = "gate/session-deadline"
	sqlStateIdleSessionTimeout = "57P05"
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
	return n
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

	case "NotificationResponse":
		// A canary: LISTEN is refused at classification, so a notification can
		// only mean the classifier was bypassed.
		return nil, fmt.Errorf("the target sent a NotificationResponse, which LISTEN's refusal makes impossible")
	}
	return nil, fmt.Errorf("unframeable backend message %q", m.Kind)
}
