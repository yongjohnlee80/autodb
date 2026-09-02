package frontdoor

import (
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Cells for the post-auth frame decision table.
//
// DELIBERATELY UNCITED. These prove the DECISION for a frame; several of the
// matrix rows they belong to also require socket-level evidence (that the
// connection is still usable after a refusal, that an unknown byte actually
// closes the stream), which arrives with the loop itself. The coverage gate
// fails on an awaiting row that is cited anywhere, and it is right to: a
// citation is a claim the row is proven, and half a row is not. The citations
// and the triage flip land together in the commit that installs the loop.

// The fast-path refusal is a REFUSAL, and the difference from a violation is
// the whole point of the row: the peer spoke the protocol correctly and asked
// for a surface that is not exposed, so the connection must survive.
func TestDispatch_FunctionCallIsRefusedAndTheSessionSurvives(t *testing.T) {
	t.Parallel()

	d, ok := dispatchFrame(&pgproto3.FunctionCall{})
	if !ok {
		t.Fatal("FunctionCall was not decided by the frame table")
	}
	if d.after != keepSession {
		t.Fatal("the fast-path refusal ended the session; it is a refusal, not a protocol violation, and the connection must stay usable")
	}
	if d.emit == nil {
		t.Fatal("no ErrorResponse: a refusal the peer cannot see is indistinguishable from the feature working")
	}
	if d.emit.Code != sqlStateFeatureNotSupported {
		t.Errorf("code = %q, want %q feature_not_supported", d.emit.Code, sqlStateFeatureNotSupported)
	}
	if d.emit.Severity != "ERROR" {
		t.Errorf("severity = %q, want ERROR — FATAL would tell the client the connection is gone when it is not", d.emit.Severity)
	}
	if d.emit.Detail != ruleNoFastpath {
		t.Errorf("DETAIL = %q, want the stable rule id %q", d.emit.Detail, ruleNoFastpath)
	}
	if d.auditKind != "fd.refused" || d.auditReason != ruleNoFastpath {
		t.Errorf("audit = %q/%q, want fd.refused/%s", d.auditKind, d.auditReason, ruleNoFastpath)
	}
	if d.closeReason != "" {
		t.Errorf("closeReason = %q on a surviving session", d.closeReason)
	}
}

// COPY sub-protocol frames are a violation, not a refusal: COPY statements are
// refused at classification, so no COPY is ever active, so the peer believes it
// is in a state that does not exist and the framing is already in doubt.
func TestDispatch_CopySubprotocolIsAFatalViolation(t *testing.T) {
	t.Parallel()

	for name, msg := range map[string]pgproto3.FrontendMessage{
		"CopyData": &pgproto3.CopyData{},
		"CopyDone": &pgproto3.CopyDone{},
		"CopyFail": &pgproto3.CopyFail{},
	} {
		t.Run(name, func(t *testing.T) {
			d, ok := dispatchFrame(msg)
			if !ok {
				t.Fatalf("%s was not decided by the frame table", name)
			}
			if d.after != endSession {
				t.Fatalf("%s did not end the session; a COPY frame outside a COPY is a protocol violation", name)
			}
			if d.emit == nil || d.emit.Code != sqlStateProtocolViolation {
				t.Fatalf("%s: code = %v, want %s protocol_violation", name, d.emit, sqlStateProtocolViolation)
			}
			if d.emit.Severity != "FATAL" {
				t.Errorf("%s: severity = %q, want FATAL — the connection is being closed", name, d.emit.Severity)
			}
			if d.emit.Detail != ruleCopySubprotocolInactive {
				t.Errorf("%s: DETAIL = %q, want %q", name, d.emit.Detail, ruleCopySubprotocolInactive)
			}
			if d.closeReason != "protocol-violation" {
				t.Errorf("%s: closeReason = %q, want protocol-violation", name, d.closeReason)
			}
		})
	}
}

// The extended-query frames tear the connection down until F2 lands (Johno's
// ruling): one FATAL 0A000, then close. Staying usable would be honest only
// with discard-through-Sync, which is F2's state machine.
func TestDispatch_ExtendedFramesTearDownWithOneError(t *testing.T) {
	t.Parallel()

	for name, msg := range map[string]pgproto3.FrontendMessage{
		"Parse":    &pgproto3.Parse{},
		"Bind":     &pgproto3.Bind{},
		"Describe": &pgproto3.Describe{},
		"Execute":  &pgproto3.Execute{},
		"Close":    &pgproto3.Close{},
		"Flush":    &pgproto3.Flush{},
		"Sync":     &pgproto3.Sync{},
	} {
		t.Run(name, func(t *testing.T) {
			d, ok := dispatchFrame(msg)
			if !ok {
				t.Fatalf("%s was not decided by the frame table", name)
			}
			if d.after != endSession {
				t.Fatalf("%s kept the session; the ruling is tear-down, because staying usable requires F2's discard-through-Sync", name)
			}
			if d.emit == nil || d.emit.Code != sqlStateFeatureNotSupported {
				t.Fatalf("%s: code = %v, want %s", name, d.emit, sqlStateFeatureNotSupported)
			}
			if d.emit.Severity != "FATAL" {
				t.Errorf("%s: severity = %q, want FATAL — the connection does not survive this", name, d.emit.Severity)
			}
			if d.emit.Detail != ruleExtendedNotImplemented {
				t.Errorf("%s: DETAIL = %q, want %q so the audit trail is greppable and the wire is unambiguous",
					name, d.emit.Detail, ruleExtendedNotImplemented)
			}
			if d.closeReason != "extended-query-not-implemented" {
				t.Errorf("%s: closeReason = %q, want its own reason rather than a generic one", name, d.closeReason)
			}
		})
	}
}

// Terminate is a clean close and draws NO frame: PostgreSQL replies to it with
// nothing at all. The session teardown it triggers (rollback, lease release) is
// the loop's, not a frame's.
func TestDispatch_TerminateClosesWithoutAFrame(t *testing.T) {
	t.Parallel()

	d, ok := dispatchFrame(&pgproto3.Terminate{})
	if !ok {
		t.Fatal("Terminate was not decided by the frame table")
	}
	if d.emit != nil {
		t.Errorf("Terminate drew a frame %v; the server answers Terminate with silence", d.emit)
	}
	if d.after != endSession {
		t.Error("Terminate did not end the session")
	}
	if d.auditKind != "fd.conn_close" || d.closeReason != "terminate" {
		t.Errorf("audit = %q reason = %q, want fd.conn_close/terminate", d.auditKind, d.closeReason)
	}
}

// Query is routed, not decided: it is the one frame with a whole-buffer engine
// unit behind it. If it ever starts being decided here, the implicit-block
// semantics have been reimplemented in the loop.
func TestDispatch_QueryIsRoutedNotDecided(t *testing.T) {
	t.Parallel()

	if _, ok := dispatchFrame(&pgproto3.Query{String: "SELECT 1"}); ok {
		t.Fatal("Query was decided by the frame table; it must be routed to the engine, which owns implicit-transaction semantics")
	}
}

// The type-byte guard. The positive half matters as much as the negative: a
// guard that rejected a valid byte would close healthy connections, and a test
// that only checked the rejections could not tell the two apart.
func TestDispatch_ValidFrontendTypeCoversTheProtocolAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, b := range []byte{'B', 'C', 'D', 'E', 'F', 'H', 'P', 'Q', 'S', 'X', 'c', 'd', 'f', 'p'} {
		if !validFrontendType(b) {
			t.Errorf("%q is a defined frontend message type but was rejected", b)
		}
	}
	// Bytes the protocol does not define, including ones that ARE defined in
	// the BACKEND direction ('R' authentication, 'T' RowDescription, 'Z'
	// ReadyForQuery) — a backend byte arriving from a client is exactly the
	// confusion this guard exists to catch.
	for _, b := range []byte{'A', 'G', 'R', 'T', 'Z', 'q', 'z', 0x00, 0xFF, '{'} {
		if validFrontendType(b) {
			t.Errorf("%q is not a frontend message type but was accepted", b)
		}
	}
}

// An unknown type byte is fatal and closes. It is never skipped: the four bytes
// after it are a length there is no reason to trust, so there is no defensible
// number of bytes to skip to find the next message.
func TestDispatch_UnknownMessageTypeIsFatalAndCloses(t *testing.T) {
	t.Parallel()

	d := unknownMessageType()
	if d.after != endSession {
		t.Fatal("an unknown message type did not close the connection; skipping it would resynchronize on data")
	}
	if d.emit == nil || d.emit.Code != sqlStateProtocolViolation {
		t.Fatalf("code = %v, want %s protocol_violation", d.emit, sqlStateProtocolViolation)
	}
	if d.emit.Severity != "FATAL" {
		t.Errorf("severity = %q, want FATAL", d.emit.Severity)
	}
	if d.emit.Detail != ruleUnknownMessageType {
		t.Errorf("DETAIL = %q, want %q", d.emit.Detail, ruleUnknownMessageType)
	}
	if d.auditKind != "fd.refused" {
		t.Errorf("auditKind = %q, want fd.refused", d.auditKind)
	}
}

// Every synthesized front-door error carries the §8a identity: accurate code,
// the stable rule id in DETAIL, remediation in HINT — and never impersonates
// the target. The uniform pre-auth denial code must not leak into this surface,
// because after authentication the front door answers accurately.
func TestDispatch_SynthesizedErrorsCarryTheGateIdentity(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for name, msg := range map[string]pgproto3.FrontendMessage{
		"FunctionCall": &pgproto3.FunctionCall{},
		"CopyData":     &pgproto3.CopyData{},
		"Parse":        &pgproto3.Parse{},
	} {
		d, _ := dispatchFrame(msg)
		if d.emit == nil {
			t.Fatalf("%s: no frame", name)
		}
		if d.emit.Detail == "" {
			t.Errorf("%s: empty DETAIL — the rule id is what distinguishes a gate error from a target error", name)
		}
		if d.emit.Hint == "" {
			t.Errorf("%s: empty HINT — §8a carries remediation", name)
		}
		if d.emit.Code == DenialSQLState {
			t.Errorf("%s: used the uniform pre-auth denial code %s; post-auth the surface answers accurately", name, DenialSQLState)
		}
		if d.emit.Severity != d.emit.SeverityUnlocalized {
			t.Errorf("%s: severity %q != unlocalized %q", name, d.emit.Severity, d.emit.SeverityUnlocalized)
		}
		if seen[d.emit.Detail] {
			t.Errorf("%s: rule id %q is shared with another decision; a shared id is not an identity", name, d.emit.Detail)
		}
		seen[d.emit.Detail] = true
	}
	if d := unknownMessageType(); seen[d.emit.Detail] {
		t.Errorf("the unknown-type rule id %q collides with another decision's", d.emit.Detail)
	}
}
