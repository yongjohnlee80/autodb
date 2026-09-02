package frontdoor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"

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
		// The type byte, BEFORE pgproto3 decodes it. An undefined byte cannot be
		// told from a transport failure once Receive has turned it into an
		// unstructured error, and it must not be: one closes the connection with
		// an accurate 08P01 and the other closes it with nothing to say.
		head, err := br.Peek(1)
		if err != nil {
			// The peer stopped talking. Nothing to report to a socket that is
			// already gone.
			*closeReason = "peer-closed"
			return nil
		}
		if !validFrontendType(head[0]) {
			d := unknownMessageType()
			l.applyDispatch(be, d, peer, closeReason)
			return nil
		}

		msg, err := be.Receive()
		if err != nil {
			*closeReason = "receive-failed"
			return nil
		}

		if d, decided := dispatchFrame(msg); decided {
			l.applyDispatch(be, d, peer, closeReason)
			if d.after == endSession {
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

// runQuery executes one simple-query buffer and frames the response. It reports
// whether the session continues.
func (l *Listener) runQuery(ctx context.Context, be *pgproto3.Backend,
	sess exec.WireSessionResult, sql, peer string, closeReason *string) bool {

	// The emitter FRAMES AND RETURNS. It must not call back into the engine on
	// this session: WireQuery holds the session's one-in-flight claim across
	// every emit, so a re-entrant call gets ErrSessionBusy by design. Anything
	// this loop needs from the engine happens after WireQuery returns.
	var emitErr error
	status, err := l.queries.WireQuery(ctx, sess.SessionID, sess.UserID, sql, hostOf(peer),
		func(m exec.WireMessage) error {
			frame, ferr := backendFrame(m)
			if ferr != nil {
				emitErr = ferr
				return ferr
			}
			if frame != nil {
				be.Send(frame)
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
	status, serr := l.queries.WireTxStatus(sess.SessionID, sess.UserID)
	if serr != nil || !validTxStatus(status) {
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

// classifyGateError maps a front-door refusal onto the §7 refusal catalogue:
// SQLSTATE, the DETAIL rule id, the HINT, and whether the connection survives.
//
// Refusals the catalogue does not name fall through to a denial-shaped answer
// rather than an invented code. Guessing a SQLSTATE would be worse than a
// generic one: a client branches on the code, and a wrong branch is a wrong
// recovery.
func classifyGateError(err error) (code, rule, hint string, fatal bool) {
	switch {
	case errors.Is(err, exec.ErrInterimTruncated):
		// Interim only: the paged producer REFUSES rather than dropping rows,
		// which is what §5 requires of anything that cannot serve a result
		// whole. It disappears with the raw path.
		return "54000", exec.InterimTruncatedRuleID,
			"the result exceeds the interim page; the raw result path removes this limit", false

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
