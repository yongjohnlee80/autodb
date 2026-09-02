package frontdoor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// Cells that drive the REAL loop over a socket.
//
// Everything here goes through an authenticated connection and reads what comes
// back, because the properties these rows name are about what a client sees:
// that a refusal leaves the connection usable, that a violation closes it, that
// a readiness byte follows a refusal. None of those can be observed from the
// decision table alone, which is why session_dispatch_test.go stops short of
// them and this file exists.
//
// UNCITED, still. The rows these prove are flipped to covered in one commit
// together with their citations, once the whole set is green — the gate fails
// both ways on drift, so a citation landing before its triage entry reddens the
// suite for everyone.

// fakeQueries is a QueryExecutor a cell can steer. It records what it was asked
// and replays a scripted response, so a cell can produce a target error, a gate
// refusal or a normal result without a database.
type fakeQueries struct {
	// mu guards every field below. The fake is written on the SERVER's session
	// goroutine and read on the test's, and it was previously ordered only by
	// accident: readUntilReady consumed the readiness byte that followed a
	// statement, which happened to sequence the two. The moment a refusal grew
	// its own readiness byte that ordering vanished and -race caught it.
	mu sync.Mutex

	// msgs are emitted in order before the status is returned.
	msgs []exec.WireMessage
	// status is the ReadyForQuery byte WireQuery reports.
	status byte
	// err, when set, is returned as a GATE refusal with nothing emitted.
	err error
	// txStatus is what WireTxStatus reports (the readiness after a refusal).
	txStatus byte
	// txErr, when set, fails the status read.
	txErr error

	// sawSQL records every statement the loop passed down.
	sawSQL []string
}

func (q *fakeQueries) WireQuery(_ context.Context, _ exec.SessionID, _ int64, sql, _ string,
	emit func(exec.WireMessage) error) (byte, error) {
	q.mu.Lock()
	q.sawSQL = append(q.sawSQL, sql)
	msgs, err, status := q.msgs, q.err, q.status
	q.mu.Unlock()

	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		if eerr := emit(m); eerr != nil {
			return 0, eerr
		}
	}
	return status, nil
}

func (q *fakeQueries) WireTxStatus(_ exec.SessionID, _ int64) (byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.txErr != nil {
		return 0, q.txErr
	}
	return q.txStatus, nil
}

// statements returns a copy of what the engine was asked to run.
func (q *fakeQueries) statements() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.sawSQL...)
}

// loopListener starts a listener with the real session loop behind it.
func loopListener(t *testing.T, q QueryExecutor) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{
		Authn:             &fakeAuth{result: goodSession()},
		Queries:           q,
		AuthFailuresPerIP: unthrottled,
	})
	return events, addr
}

// okQueries is a fake that answers one row and reports idle.
func okQueries() *fakeQueries {
	return &fakeQueries{
		status: txStatusIdle, txStatus: txStatusIdle,
		msgs: []exec.WireMessage{
			{Kind: "RowDescription", Fields: []exec.WireField{{Name: "n", TypeOID: 25, TypeSize: -1, TypeModifier: -1}}},
			{Kind: "DataRow", Values: [][]byte{[]byte("1")}},
			{Kind: "CommandComplete", Tag: "SELECT 1"},
		},
	}
}

// A simple Query runs and the client gets the response group followed by a
// SYNTHESIZED ReadyForQuery. This is the shape every other cell here builds on,
// so it is asserted first and in full.
func TestLoop_QueryRunsAndEndsWithSynthesizedReadyForQuery(t *testing.T) {
	t.Parallel()
	q := okQueries()
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var got []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the response: %v (so far: %v)", err, got)
		}
		got = append(got, msgKind(msg))
		if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
			if rfq.TxStatus != txStatusIdle {
				t.Errorf("ReadyForQuery status = %q, want %q", rfq.TxStatus, txStatusIdle)
			}
			break
		}
	}
	want := []string{"RowDescription", "DataRow", "CommandComplete", "ReadyForQuery"}
	if !sameKinds(got, want) {
		t.Fatalf("response = %v, want %v", got, want)
	}
	if saw := q.statements(); len(saw) != 1 || saw[0] != "SELECT 1" {
		t.Fatalf("the engine saw %v, want exactly [SELECT 1] — the loop must pass the buffer through unaltered", q.statements())
	}
}

// The status byte is the ENGINE's, not a constant. A loop that hard-coded 'I'
// would pass the cell above and still tell a client inside a transaction that
// it had nothing to commit.
// Witness for row 5:ReadyForQuery.
func TestLoop_ReadyForQueryCarriesTheEnginesStatus(t *testing.T) {
	t.Parallel()
	for _, want := range []byte{txStatusIdle, txStatusInTx, txStatusAborted} {
		q := okQueries()
		q.status = want
		_, addr := loopListener(t, q)
		conn, fe := authenticated(t, addr)

		fe.Send(&pgproto3.Query{String: "SELECT 1"})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		got := readUntilReady(t, fe)
		if got != want {
			t.Errorf("ReadyForQuery status = %q, want %q", got, want)
		}
		_ = conn.Close()
	}
}

// A GATE refusal is the front door's own answer: a §8a ErrorResponse with the
// rule id in DETAIL, and then a readiness byte, because the refusal did not end
// the session. The readiness comes from the engine — a refusal inside a
// transaction leaves that transaction open.
// Witness for row 5:ErrorResponse-gate-front-door.
func TestLoop_GateRefusalIsFramedWithTheGateIdentityThenReadiness(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.err = exec.ErrMultiStatement
	q.txStatus = txStatusInTx // the refusal happened inside a transaction
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1; SELECT 2"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("first frame = %T, want ErrorResponse", msg)
	}
	if e.Severity != "ERROR" {
		t.Errorf("severity = %q, want ERROR — the connection survives a refusal", e.Severity)
	}
	if e.Detail == "" {
		t.Error("no DETAIL: the rule id is what distinguishes a gate error from the target's own")
	}
	if e.Code == DenialSQLState {
		t.Errorf("code = %s, the uniform pre-auth denial; post-auth the front door answers accurately", e.Code)
	}

	// The readiness that follows says IN TRANSACTION, not idle. A loop that
	// assumed idle here would invite the client to open a second transaction.
	if got := readUntilReady(t, fe); got != txStatusInTx {
		t.Fatalf("readiness after the refusal = %q, want %q — the refusal did not close the transaction", got, txStatusInTx)
	}
}

// A TARGET error is protocol data, forwarded with its fields intact, and the
// session carries on. The distinction from the cell above is the whole of
// matrix §5's two ErrorResponse rows: one is synthesized, one is passed through,
// and a client tells them apart by DETAIL.
func TestLoop_TargetErrorIsForwardedVerbatimAndTheSessionContinues(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.status = txStatusAborted
	q.msgs = []exec.WireMessage{{Kind: "ErrorResponse", Err: &pgconn.PgError{
		Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint",
		Detail: "Key (id)=(1) already exists.", ConstraintName: "users_pkey",
		TableName: "users", SchemaName: "public", Position: 13, File: "nbtinsert.c", Line: 664, Routine: "_bt_check_unique",
	}}}
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "INSERT INTO users VALUES (1)"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the target error: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("first frame = %T, want ErrorResponse", msg)
	}
	// Every field the target populated survives. A client's own tooling reads
	// these: psql underlines from Position, an ORM maps ConstraintName.
	if e.Code != "23505" || e.ConstraintName != "users_pkey" || e.TableName != "users" ||
		e.SchemaName != "public" || e.Position != 13 || e.Routine != "_bt_check_unique" ||
		e.Detail != "Key (id)=(1) already exists." || e.File != "nbtinsert.c" || e.Line != 664 {
		t.Fatalf("the target's fields were not forwarded intact: %+v", e)
	}
	if got := readUntilReady(t, fe); got != txStatusAborted {
		t.Errorf("readiness = %q, want %q after a failed statement in a block", got, txStatusAborted)
	}
}

// The fast-path refusal must leave the connection USABLE. This is the row's
// whole point, and the reason a cell that only checked the error frame would
// not observe it: the stub refused everything with the same code and closed.
// Witness for row 4:FunctionCall.
func TestLoop_FunctionCallIsRefusedAndTheConnectionStillWorks(t *testing.T) {
	t.Parallel()
	q := okQueries()
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.FunctionCall{Function: 42})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateFeatureNotSupported || e.Detail != ruleNoFastpath {
		t.Fatalf("refusal = %s/%s, want %s/%s", e.Code, e.Detail, sqlStateFeatureNotSupported, ruleNoFastpath)
	}
	if e.Severity == "FATAL" {
		t.Fatal("the fast-path refusal is FATAL; it must be a refusal, not a violation")
	}

	// The refusal ends its own cycle with readiness (see
	// TestLoop_SurvivingRefusalEndsTheCycleWithReadiness). Consume it before
	// sending the next statement: reading past it would return this cycle's
	// readiness and the assertion below would run before the engine had been
	// asked anything.
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness closing the refusal's cycle = %q", got)
	}

	// THE LOAD-BEARING HALF: the same connection still answers a query.
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the connection did not survive the refusal: %v", err)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness after the surviving refusal = %q", got)
	}
	if saw := q.statements(); len(saw) != 1 {
		t.Fatalf("the engine saw %v; the query after the refusal did not reach it", saw)
	}
}

// A COPY sub-protocol frame is a violation: fatal, and the connection closes.
// Witness for row 4:CopyData.
func TestLoop_CopyDataIsFatalAndCloses(t *testing.T) {
	t.Parallel()
	events, addr := loopListener(t, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.CopyData{Data: []byte("row")})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the violation: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateProtocolViolation || e.Severity != "FATAL" {
		t.Fatalf("violation = %s/%s, want %s/FATAL", e.Code, e.Severity, sqlStateProtocolViolation)
	}
	if e.Detail != ruleProtocolViolation {
		t.Errorf("DETAIL = %q, want the catalogue's %q", e.Detail, ruleProtocolViolation)
	}
	// The connection is gone: nothing more arrives.
	if _, err := fe.Receive(); err == nil {
		t.Fatal("the connection survived a protocol violation")
	}
	waitFor(t, "the refusal to be audited", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == causeCopySubprotocolInactive
	})
}

// An UNDEFINED type byte is fatal and the stream closes — never skipped and
// continued. The byte is written raw because pgproto3 has no way to send one.
// Witness for row 4:Unknown-message-type-byte.
func TestLoop_UnknownMessageTypeIsFatalAndNotSkipped(t *testing.T) {
	t.Parallel()
	events, addr := loopListener(t, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// A well-formed frame whose type byte the protocol does not define, followed
	// IMMEDIATELY by a valid Query. If the loop skipped the unknown frame and
	// carried on, the Query would be answered — which is the defect this row
	// names, and why the follow-up frame is here rather than a bare assertion
	// that an error came back.
	if _, err := conn.Write([]byte{'W', 0, 0, 0, 4}); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the violation: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateProtocolViolation || e.Severity != "FATAL" {
		t.Fatalf("violation = %s/%s, want %s/FATAL", e.Code, e.Severity, sqlStateProtocolViolation)
	}
	if _, err := fe.Receive(); err == nil {
		t.Fatal("a frame arrived after the violation: the unknown byte was skipped and the stream continued")
	}
	q := okQueries()
	_ = q
	waitFor(t, "the violation to be audited", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == causeUnknownMessageType
	})
}

// The extended-protocol frames tear the connection down until F2 lands, with
// one FATAL 0A000 carrying its own rule id (Johno's ruling). UNCITED — the
// matrix has no interim row for this by design.
func TestLoop_ExtendedFrameTearsDownWithOneError(t *testing.T) {
	t.Parallel()
	_, addr := loopListener(t, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT $1::int"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateFeatureNotSupported || e.Detail != ruleExtendedNotImplemented || e.Severity != "FATAL" {
		t.Fatalf("refusal = %s/%s/%s, want %s/%s/FATAL",
			e.Severity, e.Code, e.Detail, sqlStateFeatureNotSupported, ruleExtendedNotImplemented)
	}
	// ONE error, then closed — not an error per frame.
	if _, err := fe.Receive(); err == nil {
		t.Fatal("a second frame arrived; the ruling is one error then close")
	}
}

// A backend message the front door cannot frame closes the connection loudly.
// §5's never-emitted canaries arrive exactly here — a NotificationResponse can
// only mean LISTEN's refusal was bypassed — and skipping one would hide the
// event the canary exists to catch.
// Witness for row 5:CopyInResponse.
func TestLoop_ImpossibleBackendMessageIsAFrontDoorDefectAndCloses(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.msgs = []exec.WireMessage{{Kind: "NotificationResponse",
		Notification: &pgconn.Notification{PID: 1, Channel: "c", Payload: "p"}}}
	events, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the failure: %v", err)
	}
	if e, ok := msg.(*pgproto3.ErrorResponse); !ok || e.Severity != "FATAL" {
		t.Fatalf("frame = %T (%v), want a FATAL ErrorResponse", msg, msg)
	}
	waitFor(t, "the defect to be audited under its own cause", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == ruleUnframeableMessage
	})
}

// An engine status byte outside the protocol's three is never forwarded: it is
// a claim the client acts on, and a NUL would assert a state that does not
// exist.
// Witness for row 5:ReadyForQuery.
func TestLoop_InvalidStatusByteIsNeverForwarded(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.status = 0
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			return // the connection closed without a readiness byte: correct
		}
		if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
			t.Fatalf("a ReadyForQuery with status %q reached the wire", rfq.TxStatus)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func msgKind(m pgproto3.BackendMessage) string {
	switch m.(type) {
	case *pgproto3.RowDescription:
		return "RowDescription"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery"
	case *pgproto3.ErrorResponse:
		return "ErrorResponse"
	case *pgproto3.NoticeResponse:
		return "NoticeResponse"
	case *pgproto3.EmptyQueryResponse:
		return "EmptyQueryResponse"
	}
	return "other"
}

func sameKinds(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// readUntilReady reads to the next ReadyForQuery and returns its status.
func readUntilReady(t *testing.T, fe *pgproto3.Frontend) byte {
	t.Helper()
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("waiting for ReadyForQuery: %v", err)
		}
		if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return rfq.TxStatus
		}
	}
}

// Terminate hands the session to the engine's teardown with the cause the peer
// gave, and that is what makes the row's rollback real: CloseWireSession is
// where an open transaction is rolled back and audited, so the loop's whole
// obligation is to reach it, reach it once, and name the reason honestly.
//
// The cell asserts the REASON, not merely that a close happened. A loop that
// tore down under a generic cause would satisfy "the session was closed" while
// making every teardown indistinguishable in the audit trail — and a rollback
// attributed to a deadline when the client actually said goodbye is a false
// operational record.
func TestLoop_TerminateClosesTheSessionWithItsOwnReason(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, events, addr := listenerWith(t, Options{
		Authn: f, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
	})
	conn, fe := authenticated(t, addr)

	fe.Send(&pgproto3.Terminate{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// The server answers Terminate with silence and closes.
	if msg, err := fe.Receive(); err == nil {
		t.Fatalf("Terminate drew a frame %T; the server answers it with silence", msg)
	}
	_ = conn.Close()

	waitFor(t, "the session teardown to run", func() bool {
		_, closed := f.calls()
		return len(closed) > 0
	})
	_, closed := f.calls()
	if len(closed) != 1 {
		t.Fatalf("teardown ran %d times (%v); a session is torn down exactly once", len(closed), closed)
	}
	if closed[0] != "sess-abc123/terminate" {
		t.Fatalf("teardown = %q, want %q — the cause the peer gave, so a rollback is not "+
			"attributed to a deadline the client never hit", closed[0], "sess-abc123/terminate")
	}
	waitFor(t, "the close to be audited", func() bool {
		ev, ok := find(events(), "fd.conn_close")
		return ok && ev.Reason == "terminate"
	})
}

// A protocol violation must ALSO reach the teardown, under its own cause. The
// pairing matters: if only the clean path released the session, a peer could
// hold an engine session open by ending every connection with a bad frame.
func TestLoop_ViolationAlsoTearsDownAndUnderItsOwnCause(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, _, addr := listenerWith(t, Options{
		Authn: f, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
	})
	conn, fe := authenticated(t, addr)

	fe.Send(&pgproto3.CopyData{Data: []byte("x")})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("reading the violation: %v", err)
	}
	_ = conn.Close()

	waitFor(t, "the session teardown to run", func() bool {
		_, closed := f.calls()
		return len(closed) > 0
	})
	_, closed := f.calls()
	if closed[0] != "sess-abc123/protocol-violation" {
		t.Fatalf("teardown = %q, want the violation's own cause; a session released only on the "+
			"clean path would let a peer keep one open by ending every connection badly", closed[0])
	}
}

// A lost wire tears the connection down with NO readiness byte. Asserting a
// transaction state over a connection whose wire has gone would be a claim the
// front door cannot support, and a client that believed it would send its next
// statement into a session the engine has already closed.
//
// The absence is the assertion here, so the cell reads to EOF rather than
// stopping at the first frame: a ReadyForQuery arriving after the error would
// otherwise go unnoticed.
func TestLoop_LostWireTearsDownWithoutAReadinessByte(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.err = fmt.Errorf("%w: connection reset", exec.ErrWireFaceLost)
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var sawError bool
	for {
		msg, err := fe.Receive()
		if err != nil {
			break // the connection closed, which is the contract
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			sawError = true
			if m.Severity != "FATAL" {
				t.Errorf("severity = %q, want FATAL — the session is gone", m.Severity)
			}
		case *pgproto3.ReadyForQuery:
			t.Fatalf("a ReadyForQuery (%q) followed a lost wire; the front door asserted a "+
				"transaction state over a connection that no longer exists", m.TxStatus)
		}
	}
	if !sawError {
		t.Fatal("the wire was lost and the client was told nothing at all")
	}
}

// The control: an ordinary refusal on a HEALTHY wire still gets its readiness
// byte. Without this, "no ReadyForQuery" would also be satisfied by a loop that
// had stopped sending them entirely.
func TestLoop_OrdinaryRefusalStillGetsReadiness(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.err = exec.ErrScriptTooLarge
	q.txStatus = txStatusIdle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if msg, err := fe.Receive(); err != nil {
		t.Fatalf("reading the refusal: %v", err)
	} else if _, ok := msg.(*pgproto3.ErrorResponse); !ok {
		t.Fatalf("first frame = %T, want ErrorResponse", msg)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q, want %q — a refusal on a live wire is still followed by readiness", got, txStatusIdle)
	}
}

// MF1 (Vision, PR #52 first pass). A refusal that KEEPS the session still ends a
// protocol cycle, and every cycle ends with readiness.
//
// The committed FunctionCall cell could not see this: a raw pgproto3.Frontend
// never waits for readiness, so it sends its next Query into a server happy to
// answer and the connection looks fine. A client that DOES wait — libpq's PQfn,
// and the large-object interface built on it — blocks forever. So this cell
// waits, which is the whole difference.
// Witness for row 4:FunctionCall.
func TestLoop_SurvivingRefusalEndsTheCycleWithReadiness(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.txStatus = txStatusInTx // and it is the ENGINE's state, not an assumed idle
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.FunctionCall{Function: 42})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if msg, err := fe.Receive(); err != nil {
		t.Fatalf("reading the refusal: %v", err)
	} else if _, ok := msg.(*pgproto3.ErrorResponse); !ok {
		t.Fatalf("first frame = %T, want ErrorResponse", msg)
	}
	// Before the fix this blocked until the read deadline: the loop continued
	// straight to the next Peek without ending the cycle.
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("no ReadyForQuery after a surviving refusal: %v — a client that waits for "+
			"readiness (libpq's PQfn) blocks here forever", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("frame after the refusal = %T, want ReadyForQuery", msg)
	}
	if rfq.TxStatus != txStatusInTx {
		t.Fatalf("readiness = %q, want %q — the byte is the engine's state, and a refusal "+
			"inside a transaction leaves that transaction open", rfq.TxStatus, txStatusInTx)
	}
}

// MF3 (Vision). Rows reach the client WHILE the statement is still streaming.
//
// pgproto3's Send only appends to the write buffer, so a per-row Send with one
// Flush at the end holds the whole result in memory — against a producer that
// streams unbounded, that is an OOM the size of whatever was selected. The cell
// holds the statement open after emitting past the watermark and requires that
// the client has already seen frames.
func TestLoop_OutputIsFlushedWhileTheStatementIsStillStreaming(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	q := &blockingQueries{release: release, status: txStatusIdle, txStatus: txStatusIdle}
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT big"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// The statement has NOT finished — the producer is parked — so anything that
	// arrives now arrived because the loop flushed mid-stream.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		close(release)
		t.Fatalf("no frame reached the client while the statement was still streaming: %v — "+
			"the whole result is being buffered until the statement ends", err)
	}
	if _, ok := msg.(*pgproto3.RowDescription); !ok {
		t.Fatalf("first streamed frame = %T, want RowDescription", msg)
	}
	close(release)
	_ = conn.SetReadDeadline(time.Time{})
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q", got)
	}
}

// blockingQueries emits past the output watermark, then parks until released, so
// a cell can ask what the client has seen MID-statement.
type blockingQueries struct {
	release          chan struct{}
	status, txStatus byte
}

func (b *blockingQueries) WireQuery(_ context.Context, _ exec.SessionID, _ int64, _, _ string,
	emit func(exec.WireMessage) error) (byte, error) {
	if err := emit(exec.WireMessage{Kind: "RowDescription",
		Fields: []exec.WireField{{Name: "chunk", TypeOID: 25, TypeSize: -1, TypeModifier: -1}}}); err != nil {
		return 0, err
	}
	// Enough payload to cross the watermark, so the flush is the one the
	// watermark forces rather than an incidental one.
	blob := make([]byte, 64*1024)
	for i := range blob {
		blob[i] = 'x'
	}
	for range int(pendingOutputWatermark)/len(blob) + 2 {
		if err := emit(exec.WireMessage{Kind: "DataRow", Values: [][]byte{blob}}); err != nil {
			return 0, err
		}
	}
	<-b.release
	if err := emit(exec.WireMessage{Kind: "CommandComplete", Tag: "SELECT 1"}); err != nil {
		return 0, err
	}
	return b.status, nil
}

func (b *blockingQueries) WireTxStatus(_ exec.SessionID, _ int64) (byte, error) {
	return b.txStatus, nil
}

// deadlineLoopListener is deadlineListener with a query path behind it.
func deadlineLoopListener(t *testing.T, dl deadlines, q QueryExecutor) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q,
		AuthFailuresPerIP: unthrottled, testDeadlines: &dl,
	})
	return events, addr
}

// MF2 (Vision). The idle budget must bound IDLENESS, not session lifetime.
//
// net.Conn deadlines are ABSOLUTE, so arming one at session open and never
// refreshing it turns "30 minutes idle" into a 30-minute cap on the session —
// a pooled connection doing steady work dies mid-statement having never been
// idle. This drives continuous traffic, never idling more than a fraction of
// the budget, well past the budget's own length.
func TestLoop_ABusySessionOutlivesTheIdleBudget(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 700 * time.Millisecond
	_, addr := deadlineLoopListener(t, dl, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// Three times the budget, in steps well inside it. Before the fix the
	// session stopped answering the moment the absolute deadline arrived.
	deadline := time.Now().Add(3 * dl.idle)
	n := 0
	for time.Now().Before(deadline) {
		fe.Send(&pgproto3.Query{String: "SELECT 1"})
		if err := fe.Flush(); err != nil {
			t.Fatalf("after %d statements over %v: %v — the session died while under "+
				"continuous traffic, so the budget is bounding LIFETIME rather than idleness", n, time.Since(deadline.Add(-3*dl.idle)), err)
		}
		if got := readUntilReady(t, fe); got != txStatusIdle {
			t.Fatalf("statement %d: readiness %q", n, got)
		}
		n++
		time.Sleep(dl.idle / 5)
	}
	if n < 5 {
		t.Fatalf("only %d statements ran; the cell did not exercise the budget", n)
	}
}

// MF4 (Vision). When the FRONT DOOR's own deadline fires, the client is told so
// and the audit records the front door's cause — not "the peer closed".
//
// A false operational record is worse than a missing one, because someone will
// act on it: "peer-closed" sends an operator to the client's logs for a
// disconnection the server caused.
func TestLoop_IdleExpiryIsAuditedAsTheFrontDoorsOwnDeadline(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 400 * time.Millisecond
	events, addr := deadlineLoopListener(t, dl, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// Idle past the budget and read what the server says on its way out.
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the front door closed the idle session without a frame: %v — §7 gives this "+
			"case a FATAL 57P05 under gate/session-deadline", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateIdleSessionTimeout || e.Severity != "FATAL" {
		t.Errorf("expiry = %s/%s, want %s/FATAL", e.Severity, e.Code, sqlStateIdleSessionTimeout)
	}
	if e.Detail != ruleSessionDeadline {
		t.Errorf("DETAIL = %q, want %q", e.Detail, ruleSessionDeadline)
	}
	waitFor(t, "the expiry to be audited under the front door's own cause", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.conn_close" && ev.Reason == ruleSessionDeadline {
				return true
			}
		}
		return false
	})
	// And NOT under the peer's.
	for _, ev := range events() {
		if ev.Kind == "fd.conn_close" && ev.Reason == "peer-closed" {
			t.Fatal("the front door's own deadline was audited as peer-closed; an operator would " +
				"go looking at the client for a disconnection the server caused")
		}
	}
}

// Lector's refinement on MF2. Re-arming the idle budget per message is not
// enough: the budget must not RUN during the statement either.
//
// The two are different bugs with the same symptom. A never-refreshed deadline
// kills a busy session at the budget from session open; a refreshed one that
// keeps ticking through the statement kills a single long result mid-stream. A
// cell that only sends short statements cannot tell them apart, so this one
// holds a statement open for longer than the whole budget.
func TestLoop_ALongStatementIsNotKilledByTheBetweenMessagesBudget(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 400 * time.Millisecond

	release := make(chan struct{})
	q := &blockingQueries{release: release, status: txStatusIdle, txStatus: txStatusIdle}
	_, addr := deadlineLoopListener(t, dl, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT slow"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// Hold the statement open well past the between-messages budget. The client
	// is not idle — it is waiting on a result the server is still producing.
	time.Sleep(3 * dl.idle)
	close(release)

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q", got)
	}
	// And the session is still usable afterwards: the budget resumes covering
	// the interval it names.
	fe.Send(&pgproto3.Query{String: "SELECT after"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session did not survive the long statement: %v", err)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness after the long statement = %q", got)
	}
}

// R1-1 (Vision). A client that stops reading must NOT hold the session forever.
//
// This is the regression my own previous fold introduced, and it is worth being
// precise about why: clearing the between-messages deadline for the statement's
// duration is right — a long legitimate result must not die under an IDLE budget
// — but be.Flush is a BLOCKING conn.Write, and clearing the deadline removed the
// only bound on it. A client that asks for something large and then stops reading
// fills its window, then our send buffer, and the write parks holding the session
// goroutine, the engine's one-in-flight claim, the pinned backend and its open
// transaction. The engine's statement timeouts cannot help: the target already
// produced the rows and is executing nothing.
//
// The cell asks for a large result and never reads it. The session must end.
func TestLoop_AClientThatStopsReadingDoesNotHoldTheSession(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 30 * time.Second // long, so nothing here is the idle budget
	dl.outputStall = 500 * time.Millisecond

	release := make(chan struct{})
	close(release) // stream freely; the block is the CLIENT not reading
	q := &blockingQueries{release: release, status: txStatusIdle, txStatus: txStatusIdle}
	events, addr := deadlineLoopListener(t, dl, q)

	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	fe.Send(&pgproto3.Query{String: "SELECT big"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// Deliberately never read. Before the bound, this parked forever.
	waitFor(t, "the stalled session to be closed rather than held", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.conn_close" && ev.Reason == "write-failed" {
				return true
			}
		}
		return false
	})

	// The positive control: the watermark really did engage, so the cell is
	// observing a blocked flush and not a statement that never produced output.
	var sawBackpressure bool
	for _, ev := range events() {
		if ev.Kind == "fd.backpressure_enter" {
			sawBackpressure = true
		}
	}
	if !sawBackpressure {
		t.Fatal("no backpressure was recorded; the cell did not reach the watermark flush it means to bound")
	}
}

// R1-2 (Vision). A failed WRITE is a transport fact, not a framing defect.
//
// One error for both causes made a client hanging up mid-result audit as
// frontdoor/unframeable-message and told the peer the SERVER produced something
// unforwardable — sending an operator to hunt a bug in the front door, or in the
// target. Same false-record class as the deadline audited as peer-closed, one
// function above it.
func TestLoop_AWriteFailureIsNotAuditedAsAFramingDefect(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 30 * time.Second
	dl.outputStall = 400 * time.Millisecond

	release := make(chan struct{})
	close(release)
	q := &blockingQueries{release: release, status: txStatusIdle, txStatus: txStatusIdle}
	events, addr := deadlineLoopListener(t, dl, q)

	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	fe.Send(&pgproto3.Query{String: "SELECT big"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stalled write to end the session", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.conn_close" && ev.Reason == "write-failed" {
				return true
			}
		}
		return false
	})
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == ruleUnframeableMessage {
			t.Fatal("a failed write was audited as frontdoor/unframeable-message — that blames the " +
				"front door's framing, or the target, for a client that stopped reading")
		}
	}
}

// MF10 (lector). A peer that begins a message and stops is NOT idle, and §7
// gives that its own budget and its own identity. Reporting a frame stall as
// 57P05 tells an operator the client went quiet when it actually went slow —
// and the two have different causes and different fixes.
func TestLoop_APartialFrameStallsUnderItsOwnBudgetAndIdentity(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 30 * time.Second // long: nothing here may be the idle budget
	dl.frameStall = 400 * time.Millisecond
	events, addr := deadlineLoopListener(t, dl, okQueries())

	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// A Query header promising a body, and then silence. The message has STARTED,
	// so the idle budget no longer applies.
	if _, err := conn.Write([]byte{'Q', 0, 0, 0, 40}); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the front door closed a stalled frame without a frame: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Code != sqlStateConnectionFailure || e.Detail != ruleFrameStall {
		t.Fatalf("stall = %s/%s, want %s/%s — a half-sent message is not an idle session",
			e.Code, e.Detail, sqlStateConnectionFailure, ruleFrameStall)
	}
	waitFor(t, "the stall to be audited under its own cause", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.conn_close" && ev.Reason == ruleFrameStall {
				return true
			}
		}
		return false
	})
	for _, ev := range events() {
		if ev.Kind == "fd.conn_close" && ev.Reason == ruleSessionDeadline {
			t.Fatal("a partial frame was audited as an idle-session deadline; the two budgets have " +
				"different causes and an operator would chase the wrong one")
		}
	}
}

// MF8 (lector). A burst of large NOTICES must reach the watermark. The estimate
// ignored Notice and error payloads entirely, so a producer emitting megabytes
// of them crossed no watermark and streamed nothing — the buffer grew with
// output the accounting could not see.
func TestLoop_NoticePayloadsCountTowardTheOutputWatermark(t *testing.T) {
	t.Parallel()
	big := make([]byte, 96*1024)
	for i := range big {
		big[i] = 'n'
	}
	var msgs []exec.WireMessage
	for range int(pendingOutputWatermark)/len(big) + 2 {
		msgs = append(msgs, exec.WireMessage{Kind: "NoticeResponse",
			Notice: &pgconn.Notice{Severity: "NOTICE", Code: "01000", Message: string(big)}})
	}
	msgs = append(msgs, exec.WireMessage{Kind: "CommandComplete", Tag: "SELECT 0"})

	q := &fakeQueries{msgs: msgs, status: txStatusIdle, txStatus: txStatusIdle}
	events, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT noisy"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q", got)
	}
	var sawBackpressure bool
	for _, ev := range events() {
		if ev.Kind == "fd.backpressure_enter" {
			sawBackpressure = true
		}
	}
	if !sawBackpressure {
		t.Fatalf("megabytes of notices crossed no watermark — the accounting cannot see the " +
			"payload that is filling the buffer")
	}
}

// The second frame of a pipelined pair must REACH THE DECISION TABLE, not sit
// in pgproto3's buffer unseen.
//
// F1 has no extended-protocol vocabulary to pipeline, so this proves the same
// property with frames it does have: a Query answered normally, and a Parse
// behind it in the SAME flush that must still be refused on its own terms. If
// the second frame were stranded the session would go quiet instead — which is
// exactly what a real segment (Parse+Bind+Execute+Sync in one flush) did.
func TestLoop_ASecondPipelinedFrameStillReachesItsDecision(t *testing.T) {
	t.Parallel()
	events, addr := loopListener(t, okQueries())
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// The Query's own cycle first, ending in readiness.
	sawReady := false
	for !sawReady {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the first statement of a pipelined pair: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			sawReady = true
		}
	}

	// Then the SECOND frame's decision, which only arrives if the loop ever saw it.
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the second frame of the pipelined pair was never decided: %v — it was "+
			"accepted by the client, never seen by the loop, and nobody was told", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want the extended-protocol refusal", msg)
	}
	if e.Detail != ruleExtendedNotImplemented {
		t.Fatalf("DETAIL = %q, want %q", e.Detail, ruleExtendedNotImplemented)
	}
	waitFor(t, "the pipelined second frame to be audited", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == ruleExtendedNotImplemented
	})
}

// MF1 (lector, PR #59 r0). The reader is shared with auth, and auth's Backend
// reads ahead exactly as the session's does.
//
// A client that writes its PasswordMessage and the START of a Query in ONE TLS
// write lets runAuth's Receive consume the Query's type byte and length while
// the message-start callback is still nil. The loop then arms the idle budget
// unconditionally and the type byte cannot re-trigger — so a half-sent message
// waits under the budget for a client that is not asking, instead of the one for
// a client that went slow mid-message.
//
// This is the assumption I flagged as most worth attacking when I sent the PR.
// It was worth attacking.
func TestLoop_AFrameReadAheadDuringAuthStillGetsTheFrameStallBudget(t *testing.T) {
	t.Parallel()
	dl := defaultDeadlines()
	dl.idle = 3 * time.Second // long: nothing here may be charged to idleness
	dl.frameStall = 300 * time.Millisecond
	events, addr := deadlineLoopListener(t, dl, okQueries())

	conn, fe := startupTo(t, addr, defaultParams())
	defer func() { _ = conn.Close() }()
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}

	// ONE write: the password, then a Query header promising a body that never
	// comes. Auth's Receive will take the password and read ahead into the Query.
	var one []byte
	one = append(one, 'p')
	pw := []byte("good\x00")
	n := len(pw) + 4
	one = append(one, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	one = append(one, pw...)
	one = append(one, 'Q', 0, 0, 0, 40) // 36 bytes of body promised, none sent
	if _, err := conn.Write(one); err != nil {
		t.Fatalf("the combined write: %v", err)
	}

	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	start := time.Now()
	msg, err := fe.Receive()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the stalled frame was closed without a frame: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse", msg)
	}
	if e.Detail != ruleFrameStall {
		t.Fatalf("DETAIL = %q, want %q — a message half-read during AUTH is still a message "+
			"in progress, and charging it to idleness tells an operator the client went quiet "+
			"when it went slow", e.Detail, ruleFrameStall)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the stall took %v, which is the %v IDLE budget rather than the %v frame-stall "+
			"budget: the frame read ahead during auth lost its budget entirely", elapsed, dl.idle, dl.frameStall)
	}
	waitFor(t, "the stall to be audited under its own cause", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.conn_close" && ev.Reason == ruleFrameStall {
				return true
			}
		}
		return false
	})
}
