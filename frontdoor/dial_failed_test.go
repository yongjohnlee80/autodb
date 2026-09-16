package frontdoor

import (
	"crypto/x509"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE CLIENT CONTRACT FOR A REQUEST WHOSE BACKEND COULD NOT BE OPENED.
//
// Four properties, and each of them is a separate way the shape can be wrong:
//
//  1. The frame is EXACTLY one fixed SQLSTATE, severity and literal message,
//     the same for every stage a dial can fail at.
//  2. Nothing from the raw cause reaches any field of it.
//  3. The stage and the raw cause DO reach the audit trail, because an
//     operator has three different repairs to choose between.
//  4. Recovery follows the protocol: in the extended protocol the client's
//     remaining frames are discarded through its own matching Sync, and then
//     there is ONE ReadyForQuery. Emitting readiness earlier would tell a
//     pipelining client the sequence finished while frames it had already sent
//     were still in flight.
//
// The cells below are the unit half, driven through the real loop over a real
// socket with a scripted engine. The live half — a real pgx client against a
// real PostgreSQL target — is in dial_failed_pg_test.go, because the property
// that actually decides the SQLSTATE is whether a REAL driver keeps the
// session usable, and no amount of frame assertion answers that.

// dialCause is a failure whose text names things the client must never learn:
// a host, a port, and the fact that a socket was refused.
func dialCause() error {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 5432},
		Err:  errors.New("connect: connection refused"),
	}
}

// causeTokens are the substrings that must not appear anywhere in the frame.
// They are taken from dialCause's text rather than guessed, so a reworded
// cause cannot quietly stop being checked for.
var causeTokens = []string{"203.0.113.7", "5432", "refused", "tcp"}

// assertDialFailedFrame checks the whole of the client's half of the contract.
func assertDialFailedFrame(t *testing.T, e *pgproto3.ErrorResponse) {
	t.Helper()
	if e.Severity != "ERROR" || e.SeverityUnlocalized != "ERROR" {
		t.Errorf("severity = %q/%q, want ERROR/ERROR — FATAL announces that the "+
			"backend is closing the connection, and this one is not",
			e.Severity, e.SeverityUnlocalized)
	}
	if e.Code != DialFailedSQLState {
		t.Errorf("SQLSTATE = %q, want %q", e.Code, DialFailedSQLState)
	}
	if e.Code[:2] == "08" {
		// A DESIGN GUARD RATHER THAN A MEASUREMENT, and the difference is
		// stated because the measurement came out the other way: neither pgx
		// v5 nor pgjdbc 42.7.4 closes the session on an ERROR-severity 08006
		// today. Class 08 is nonetheless the class clients and POOLS attach
		// their own recovery rules to, several of them on the two-character
		// prefix alone, so a code in it makes the promise "the session
		// survives" depend on whatever sits in front of the driver. This
		// stops the choice drifting there on the strength of two versions
		// that happen to behave.
		t.Errorf("SQLSTATE %q is in class 08; the survival of the session must not "+
			"depend on each client's and each pool's class-08 recovery policy", e.Code)
	}
	if e.Message != DialFailedMessage {
		t.Errorf("message = %q, want the fixed literal %q", e.Message, DialFailedMessage)
	}
	if e.Detail != DialFailedRule {
		t.Errorf("detail = %q, want the stable rule id %q — DETAIL carries the rule, "+
			"never the cause", e.Detail, DialFailedRule)
	}
	if e.Hint != DialFailedHint {
		t.Errorf("hint = %q, want %q", e.Hint, DialFailedHint)
	}
	for _, field := range []struct{ name, value string }{
		{"Message", e.Message}, {"Detail", e.Detail}, {"Hint", e.Hint},
		{"Where", e.Where}, {"InternalQuery", e.InternalQuery},
		{"SchemaName", e.SchemaName}, {"TableName", e.TableName},
		{"ColumnName", e.ColumnName}, {"ConstraintName", e.ConstraintName},
		{"File", e.File}, {"Routine", e.Routine},
	} {
		for _, tok := range causeTokens {
			if strings.Contains(strings.ToLower(field.value), strings.ToLower(tok)) {
				t.Errorf("%s = %q carries %q from the raw dial cause; the client learns "+
					"the estate's topology from it", field.name, field.value, tok)
			}
		}
	}
}

// A simple Query whose backend cannot be acquired gets ONE error and ONE
// readiness byte, and the session is still there afterwards.
func TestDialFailed_SimpleQueryGetsTheFixedFrameThenOneReadyForQuery(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.err = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	events, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the response: %v (so far: %v)", err, kinds)
		}
		kinds = append(kinds, msgKind(msg))
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			errFrame = e
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(kinds, []string{"ErrorResponse", "ReadyForQuery"}) {
		t.Fatalf("response = %v, want exactly [ErrorResponse ReadyForQuery]", kinds)
	}
	assertDialFailedFrame(t, errFrame)

	// THE SESSION IS STILL USABLE. This is the promise the whole shape exists
	// to keep, and it is the one a FATAL severity or a class 08 code would
	// break while every field assertion above still passed.
	q.mu.Lock()
	q.err = nil
	q.mu.Unlock()
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var after []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the session did not survive the dial failure: %v (so far: %v)", err, after)
		}
		after = append(after, msgKind(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(after, []string{"RowDescription", "DataRow", "CommandComplete", "ReadyForQuery"}) {
		t.Fatalf("the statement after the dial failure answered %v, want a whole result", after)
	}

	// THE AUDIT HAS WHAT THE WIRE DOES NOT.
	var found bool
	for _, ev := range events() {
		if ev.Kind != EventDialFailed {
			continue
		}
		found = true
		if ev.Reason != DialFailedRule {
			t.Errorf("audit reason = %q, want %q", ev.Reason, DialFailedRule)
		}
		if !strings.Contains(ev.Detail, "stage=") || !strings.Contains(ev.Detail, "203.0.113.7") {
			t.Errorf("audit detail = %q, want the stage and the raw cause — the operator "+
				"has three different repairs to choose between and the client must not "+
				"be able to tell them apart", ev.Detail)
		}
	}
	if !found {
		t.Errorf("no %s event; the cause and the stage are withheld from the client on "+
			"the understanding that the operator gets them, and nothing recorded them",
			EventDialFailed)
	}
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == DialFailedRule {
			t.Error("a dial failure was filed as a refusal; a target that could not be " +
				"reached judged nothing, and counting it among refusals hides a target " +
				"outage in the policy numbers")
		}
	}
}

// EVERY STAGE PRODUCES THE IDENTICAL FRAME. This is the property that makes the
// surface useless as an oracle: a caller who could tell a name that will not
// resolve from a certificate that expired from an upstream password that
// changed would be reading our estate off our error surface.
func TestDialFailed_EveryStageProducesTheIdenticalFrame(t *testing.T) {
	t.Parallel()

	causes := map[string]error{
		"resolve":      &net.DNSError{Err: "no such host", Name: "db.internal.example"},
		"connect":      dialCause(),
		"tls":          x509.UnknownAuthorityError{},
		"authenticate": &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: `password authentication failed for user "autodb"`},
		"startup":      &pgconn.PgError{Severity: "FATAL", Code: "53300", Message: "sorry, too many clients already"},
		"settings":     errors.New(`SET client_encoding failed on host db.internal.example`),
	}

	var first *pgproto3.ErrorResponse
	var firstName string
	for name, cause := range causes {
		q := okQueries()
		q.err = exec.NewDialFailure(cause)
		_, addr := loopListener(t, q)
		conn, fe := authenticated(t, addr)

		fe.Send(&pgproto3.Query{String: "SELECT 1"})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		var got *pgproto3.ErrorResponse
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("%s: reading the response: %v", name, err)
			}
			if e, ok := msg.(*pgproto3.ErrorResponse); ok {
				got = e
			}
			if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
				break
			}
		}
		_ = conn.Close()
		if got == nil {
			t.Fatalf("%s: no ErrorResponse", name)
		}
		assertDialFailedFrame(t, got)
		// The upstream causes above carry a hostname, a role name and an
		// upstream SQLSTATE of their own. None of them may survive.
		for _, leak := range []string{"db.internal.example", "autodb", "28P01", "53300", "no such host"} {
			for _, v := range []string{got.Message, got.Detail, got.Hint, got.Code} {
				if strings.Contains(v, leak) {
					t.Errorf("%s: the frame carries %q from upstream", name, leak)
				}
			}
		}
		if first == nil {
			first, firstName = got, name
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Errorf("the %s stage renders differently from the %s stage:\n %+v\n %+v",
				name, firstName, got, first)
		}
	}
}

// RECOVERY FOLLOWS THE PROTOCOL: one error, then every remaining extended frame
// is discarded through the client's OWN matching Sync, then ONE ReadyForQuery.
//
// Flush is deliberately in the middle of the pipeline. It is NOT a segment
// boundary, so it must not end the discard and must not produce a second
// readiness byte; a loop that treated it as one would tell a pipelining client
// the sequence completed while its Execute was still unanswered.
func TestDialFailed_ExtendedInputIsDiscardedThroughTheMatchingSync(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.parseErr = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// A whole pipelined segment, sent in one write, exactly as a driver that
	// does not wait for each answer sends it.
	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{PreparedStatement: "s1", DestinationPortal: "p1"})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "p1"})
	fe.Send(&pgproto3.Execute{Portal: "p1"})
	fe.Send(&pgproto3.Flush{})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers: %v (so far: %v)", err, kinds)
		}
		kinds = append(kinds, msgKind(msg))
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			errFrame = e
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(kinds, []string{"ErrorResponse", "ReadyForQuery"}) {
		t.Fatalf("the segment answered %v, want exactly [ErrorResponse ReadyForQuery] — "+
			"one refusal for one segment, and readiness only at the client's own Sync", kinds)
	}
	assertDialFailedFrame(t, errFrame)

	// NOTHING BETWEEN THE ERROR AND THE SYNC REACHED THE ENGINE. The discard is
	// not cosmetic: a Bind or an Execute acted on after the segment failed
	// would run work the client believes was abandoned.
	calls := q.calls()
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "Parse:") {
		t.Fatalf("the engine saw %v, want the Parse first", calls)
	}
	for _, c := range calls[1:] {
		if c != "Sync" {
			t.Errorf("the engine saw %q after the failed Parse; every frame between the "+
				"error and the matching Sync must be discarded", c)
		}
	}
	if calls[len(calls)-1] != "Sync" {
		t.Errorf("the engine saw %v, want the client's Sync to end the discard", calls)
	}

	// AND THE SESSION IS STILL USABLE, in the extended protocol too.
	q.mu.Lock()
	q.parseErr = nil
	q.mu.Unlock()
	fe.Send(&pgproto3.Parse{Name: "s2", Query: "SELECT 1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var after []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the session did not survive the dial failure: %v (so far: %v)", err, after)
		}
		after = append(after, msgKind(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	// The scripted engine emits nothing of its own at Sync, so the whole of
	// what a healthy segment produces here is the readiness byte. What matters
	// is that it arrives and carries no second refusal: the live cells prove
	// the same session goes on to return real rows from a real target.
	if !sameKinds(after, []string{"ReadyForQuery"}) {
		t.Fatalf("the segment after the dial failure answered %v, want [ReadyForQuery]", after)
	}
}

// A dial failure at EXECUTE — after the segment has already produced answers —
// is the same one error and the same one readiness byte. It is a separate cell
// because it is a separate code path: the streaming renderer, not the one that
// answers a frame that queued nothing.
func TestDialFailed_AFailureAtExecuteStillEndsInOneReadyForQuery(t *testing.T) {
	t.Parallel()

	q := okQueries()
	q.executeErr = exec.NewDialFailure(dialCause())
	q.txStatus = txStatusIdle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{PreparedStatement: "", DestinationPortal: ""})
	fe.Send(&pgproto3.Execute{Portal: ""})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	errFrames, readies := 0, 0
	var errFrame *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			errFrames++
			errFrame = m
		case *pgproto3.ReadyForQuery:
			readies++
		}
		if readies > 0 {
			break
		}
	}
	if errFrames != 1 {
		t.Errorf("the segment produced %d ErrorResponses, want exactly 1", errFrames)
	}
	if errFrame == nil {
		t.Fatal("no ErrorResponse for a dial failure at Execute")
	}
	assertDialFailedFrame(t, errFrame)
}
