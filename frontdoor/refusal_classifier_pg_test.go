package frontdoor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// A REFUSAL AUTODB ORIGINATES MUST NOT BE REPORTED AS THE TARGET'S WIRE DYING.
//
// The raw face classified EVERY error that was not a consumer-emit failure as a
// poisoned connection: audit `wire_raw_face_lost`, transfer-close the session,
// and answer the client `08P01` FATAL. Its comment enumerated two causes — a
// transport failure, and control reaching the raw face, which the gate makes
// impossible — and treated the rest as unreachable.
//
// A GOLIB GUARD REFUSING A CALL IS A THIRD CAUSE, and it is the ordinary one.
// The guard refuses BEFORE touching the wire, so the connection is in exactly
// the state it was in a moment earlier. Reporting that as the target's transport
// failing tells a conformant client its connection died, and `08P01`
// protocol_violation tells it that it broke the protocol — for a sequence real
// PostgreSQL answers normally.
//
// THE MEASURED INSTANCE is Parse-then-simple-Query: PostgreSQL replies
// `ParseComplete RowDescription DataRow CommandComplete ReadyForQuery(I)`, and
// autodb replied `ErrorResponse(08P01 FATAL)` and closed, with the audit detail
// carrying golib's own "an extended segment is in flight" as "the session's wire
// failed".
//
// SCOPE OF THE CLAIM — ONE SENTINEL, NOT A CATEGORY. The engine classifies
// golibpg.ErrSegmentInFlight as a refusal and NOTHING ELSE; every other error,
// including any other golib error, keeps the fatal wire-lost default, which is
// correct for a transport failure or a poisoned face. This cell is the evidence
// for that single sentinel and claims nothing about golib refusals as a class —
// widening it would require enumerating golib's refusal shapes, which is a
// golib-side question this repo cannot answer.
//
// It also does not claim the sequence becomes SUPPORTED: whether autodb adopts
// PostgreSQL's mid-segment-Query behaviour is an open contract question. It
// claims only that our refusal is reported as ours.
func TestRefusal_AGolibRefusalIsNotReportedAsAWireFailure(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, events, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})
	conn, fe := pgClientWithConn(t, addr, secret, database)

	// Parse opens an extended segment; the simple Query then reaches the raw face
	// while golib's guard holds the wire, and the guard refuses it.
	fe.Send(&pgproto3.Parse{Name: "rc", Query: "SELECT 1"})
	fe.Send(&pgproto3.Query{String: "SELECT 42"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// DRAIN THROUGH THE READINESS, not to the first frame of interest. Stopping
	// at the ErrorResponse leaves the readiness that follows it on the wire, and
	// the next read then consumes a STALE one — which desynchronises everything
	// after it and reads as "the follow-up statement did not run". The cell's own
	// recovery assertion caught that; the frames were never the problem.
	var seen []string
	var refusal *pgproto3.ErrorResponse
	for i := 0; i < 8; i++ {
		m, err := fe.Receive()
		if err != nil {
			seen = append(seen, "RECV-ERR:"+err.Error())
			break
		}
		seen = append(seen, strings.TrimPrefix(fmt.Sprintf("%T", m), "*pgproto3."))
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			refusal = e
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	// PREMISE: a refusal actually reached the client. If the sequence were
	// answered normally this cell would be testing a path that no longer exists,
	// and should be retargeted rather than quietly passing.
	if refusal == nil {
		t.Fatalf("PREMISE FAILED: no refusal reached the client (got %v) — the "+
			"sequence is no longer refused here and this cell needs retargeting",
			seen)
	}

	if refusal.Severity == "FATAL" {
		t.Errorf("our own refusal was reported FATAL (%s: %s) — the guard refuses "+
			"before touching the wire, so the connection is exactly as it was",
			refusal.Code, refusal.Message)
	}
	if refusal.Code == sqlStateProtocolViolation {
		t.Errorf("our own refusal was reported as %s protocol_violation — that tells "+
			"a conformant client it broke the protocol, for a sequence PostgreSQL "+
			"answers normally", refusal.Code)
	}
	if refusal.Code != sqlStateFeatureNotSupported {
		t.Errorf("refusal code = %q, want %q feature_not_supported — this is a "+
			"sequence autodb does not support, which is what 0A000 says",
			refusal.Code, sqlStateFeatureNotSupported)
	}
	// The client must not be told the TARGET's connection failed.
	if strings.Contains(strings.ToLower(refusal.Message), "wire failed") ||
		strings.Contains(strings.ToLower(refusal.Message), "connection to the target failed") {
		t.Errorf("the refusal message blames the target's connection: %q", refusal.Message)
	}

	// THE AUDIT MUST NOT SAY THE WIRE WAS LOST EITHER. The client's story and the
	// audit's have to agree, and the audit is where an operator looks first.
	for _, e := range events() {
		if e.Reason == "frontdoor/wire-face-lost" {
			t.Errorf("audited as %q with detail %q — a guard refusal is not a lost wire",
				e.Reason, e.Detail)
		}
	}

	// AND THE CONNECTION MUST STILL WORK. This is the half that makes "a refusal,
	// not a violation" true rather than merely claimed: a non-fatal code on a
	// connection that dies anyway is the same outage with a politer label. The
	// guard refused before touching the wire, so the client ends its segment with
	// Sync and carries on.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session was closed by its own refusal: %v", err)
	}
	// The Sync ends the segment the Parse opened and delivers its queued answer.
	afterSync := readUntil(t, conn, fe, untilReady)
	if len(afterSync) == 0 {
		t.Fatal("nothing followed the Sync — the session did not survive the refusal")
	}
	if _, ok := firstOfType[*pgproto3.ParseComplete](afterSync); !ok {
		t.Errorf("the Sync delivered %v — the Parse that opened the segment was "+
			"refused along with the Query it never belonged to", kindsOf(afterSync))
	}
	after := query(t, fe, "SELECT 42")
	if hasError(after) {
		t.Fatalf("the session was not usable after its own refusal: %v", errorText(after))
	}
	dr, ok := firstOfType[*pgproto3.DataRow](after)
	if !ok || len(dr.Values) != 1 || string(dr.Values[0]) != "42" {
		t.Fatalf("the follow-up statement returned no row with 42 (frames=%v) — an "+
			"errorless reply proves the wire drained, not that a statement ran",
			kindsOf(after))
	}
}
