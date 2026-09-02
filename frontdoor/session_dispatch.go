package frontdoor

import (
	"github.com/jackc/pgx/v5/pgproto3"
)

// THE POST-AUTH FRAME DISPATCH (F1's wire loop, matrix §4).
//
// This file decides what happens to one frontend frame and says nothing about
// how it is read or written. That split is deliberate: the decisions here are
// the ones the protocol matrix rules on, and a decision expressed as a value
// can be asserted directly by a cell, whereas the same decision buried in the
// loop's control flow can only be observed through a socket.
//
// It is also what lets F2 land without rewriting F1. The extended-protocol
// frames (Parse/Bind/Describe/Execute/Close/Flush/Sync) have exactly ONE entry
// here today — a refusal (§ interimExtendedRefusal). When F2's arms land they
// replace that entry; they do not restructure the loop, and there is never a
// second loop. Segment and object lifetime state stays engine-side, so nothing
// in this file needs to remember anything between frames.

// frameAfter says what the loop does once a decision's frame (if any) is on
// the wire. The three values are the whole vocabulary: a frame is either the
// end of the connection or it is not, and "discard the rest of the segment"
// deliberately does NOT appear — post-error discard-through-Sync is matrix row
// 4:discard, which is F2's, and a temporary one built here would be a second
// state machine F2 has to delete (zen's objection, agreed by jarvis).
type frameAfter int

const (
	// keepSession continues the loop: the connection stays usable. A refusal
	// that keeps the session is a REFUSAL; one that does not is a violation.
	keepSession frameAfter = iota
	// endSession closes the connection after the frame is flushed.
	endSession
)

// dispatch is the decision for one frame: an optional frame to emit, whether
// the session continues, and the INTERNAL audit vocabulary for both. The audit
// fields never reach the wire (matrix §1.2, Event.Reason/Detail).
type dispatch struct {
	// emit is the frame to send before acting on after, or nil to send none.
	emit *pgproto3.ErrorResponse
	// after says whether the session continues once emit is flushed.
	after frameAfter
	// auditKind is the fd.* event this decision records, or "" for none.
	auditKind string
	// auditReason is the internal rule id or cause. Never sent to the peer.
	auditReason string
	// auditDetail is free text for the audit row: the varying particulars a
	// stable rule id must not absorb. Never sent to the peer.
	auditDetail string
	// closeReason names the connection's cause of death for fd.conn_close,
	// set only when after is endSession.
	closeReason string
}

// Front-door rule ids for post-auth decisions. They are the stable identity a
// DETAIL carries and an audit row is greppable by (matrix §1.2/§8a); unlike
// the pre-auth denialReason vocabulary these DO reach the wire, because after
// authentication the surface may say accurately what it refused — §8a exists
// precisely so a caller can tell a gate refusal from a target error.
const (
	// ruleNoFastpath refuses the legacy function-call sub-protocol. It bypasses
	// text classification BY CONSTRUCTION — there is no SQL to classify — so it
	// can never be gated, only refused (matrix row 4:FunctionCall).
	ruleNoFastpath = "frontdoor/no-fastpath"

	// ruleProtocolViolation is the catalogue's id for "out-of-state / unknown
	// message" (matrix §7): a fatal 08P01 whose stream cannot be resynchronized.
	// It is the WIRE identity for both the COPY-out-of-state and unknown-type
	// cases, because §7 names one id for that class and a DETAIL the catalogue
	// does not document is one an operator cannot look up.
	//
	// The finer cause is not lost — it travels in the AUDIT reason below, which
	// is the operator-facing identity and is free to be as specific as the cause
	// actually is. Wire identity follows the published catalogue; audit identity
	// follows the defect.
	ruleProtocolViolation = "frontdoor/protocol-violation"

	// The internal audit causes behind ruleProtocolViolation. These never reach
	// the wire (matrix §1.2, Event.Reason).
	causeCopySubprotocolInactive = "frontdoor/copy-subprotocol-inactive"
	causeUnknownMessageType      = "frontdoor/unknown-message-type"
)

// SQLSTATEs used post-auth. 28000 (the uniform pre-auth denial) is deliberately
// absent: after authentication the surface answers accurately.
const (
	// sqlStateFeatureNotSupported is 0A000 — the feature is not here. A
	// refusal, so the peer may keep using the connection unless something else
	// says otherwise.
	sqlStateFeatureNotSupported = "0A000"

	// sqlStateProtocolViolation is 08P01 — the peer broke the protocol. Always
	// fatal: a stream whose framing is in doubt cannot be reasoned about, and
	// PostgreSQL closes rather than guessing where the next message starts.
	sqlStateProtocolViolation = "08P01"
)

// gateError builds a front-door-synthesized ErrorResponse in the §8a identity
// (matrix §1.2): accurate severity and code, the stable rule id in DETAIL, and
// remediation in HINT. It never impersonates the target — a caller that needs
// a target error forwards the target's own fields verbatim instead.
func gateError(severity, code, message, ruleID, hint string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            severity,
		SeverityUnlocalized: severity,
		Code:                code,
		Message:             message,
		Detail:              ruleID,
		Hint:                hint,
	}
}

// validFrontendType reports whether b is a frontend message type byte the v3
// protocol defines. It is the guard for matrix row 4:Unknown-message-type-byte.
//
// It exists because pgproto3 reports an unknown type as an UNSTRUCTURED error
// (`fmt.Errorf("unknown message type: %c")`, backend.go), which cannot be told
// from a transport failure without matching on its text — and a string match
// would silently start passing every unknown byte through as a transport error
// the day pgx rewords it. Peeking the byte before pgproto3 sees it turns the
// question into one this package answers for itself.
//
// The set is the frontend half of the v3 message set. 'p' is the
// authentication-response byte, which is context-dependent post-auth and has
// no meaning here, but it is a DEFINED byte: a peer sending one is out of
// sequence, not speaking a protocol we do not know, so it is not this row's
// concern and pgproto3's own decode refuses it.
func validFrontendType(b byte) bool {
	switch b {
	case 'B', // Bind
		'C', // Close
		'D', // Describe
		'E', // Execute
		'F', // FunctionCall
		'H', // Flush
		'P', // Parse
		'Q', // Query
		'S', // Sync
		'X', // Terminate
		'c', // CopyDone
		'd', // CopyData
		'f', // CopyFail
		'p': // authentication response
		return true
	default:
		return false
	}
}

// unknownMessageType is the decision for a type byte the protocol does not
// define: a fatal 08P01 and close, NEVER skipped-and-continued.
//
// Skipping is not available even in principle. The byte is the first of five
// header bytes whose remaining four are a length we have no reason to trust —
// so there is no defensible number of bytes to skip to reach the next message,
// and a guess would resynchronize on data. PostgreSQL closes here for the same
// reason (matrix row 4:Unknown-message-type-byte).
func unknownMessageType() dispatch {
	return dispatch{
		emit: gateError("FATAL", sqlStateProtocolViolation,
			"unknown message type", ruleProtocolViolation,
			"the connection speaks the PostgreSQL v3 frontend protocol; the message type byte is not one it defines"),
		after:       endSession,
		auditKind:   "fd.refused",
		auditReason: causeUnknownMessageType,
		closeReason: "protocol-violation",
	}
}

// dispatchFrame decides one decoded frame.
//
// Query is absent by design: it is the one frame with an ENGINE unit behind it
// (a whole-buffer implicit-transaction block, matrix row 4:Query), so the loop
// routes it rather than deciding it. Everything decidable from the frame ALONE
// is decided here, which is what makes those decisions testable without a
// socket, an engine or a target.
func dispatchFrame(msg pgproto3.FrontendMessage) (dispatch, bool) {
	switch msg.(type) {
	// The legacy fast-path. A refusal, not a violation: the peer is speaking
	// the protocol correctly and asking for a surface this front door does not
	// expose, so the connection SURVIVES. That distinction is the whole of
	// matrix row 4:FunctionCall — a cell proving the refusal must also prove
	// the connection still answers afterwards, or it has not observed the row.
	case *pgproto3.FunctionCall:
		return dispatch{
			emit: gateError("ERROR", sqlStateFeatureNotSupported,
				"the function-call fast-path is not supported", ruleNoFastpath,
				"use a SQL statement; the fast-path carries no statement text to classify"),
			after:       keepSession,
			auditKind:   "fd.refused",
			auditReason: ruleNoFastpath,
		}, true

	// COPY sub-protocol frames. COPY the STATEMENT is refused at
	// classification, so a COPY sub-protocol is never active on this
	// connection, so one of these arriving means the peer believes it is in a
	// copy-in state that does not exist. The framing is therefore already in
	// doubt: violation, fatal, close (matrix row 4:CopyData).
	case *pgproto3.CopyData, *pgproto3.CopyDone, *pgproto3.CopyFail:
		return dispatch{
			emit: gateError("FATAL", sqlStateProtocolViolation,
				"COPY sub-protocol message outside a COPY", ruleProtocolViolation,
				"COPY statements are refused at classification, so no COPY sub-protocol is ever active"),
			after:       endSession,
			auditKind:   "fd.refused",
			auditReason: causeCopySubprotocolInactive,
			closeReason: "protocol-violation",
		}, true

	// A clean close. No frame: PostgreSQL sends nothing in reply to Terminate,
	// and the session's own teardown (an open transaction rolled back and
	// audited, the lease and charges released) is the caller's, not a frame's.
	case *pgproto3.Terminate:
		return dispatch{
			after:       endSession,
			auditKind:   "fd.conn_close",
			auditReason: "terminate",
			closeReason: "terminate",
		}, true
	}
	return dispatch{}, false
}
