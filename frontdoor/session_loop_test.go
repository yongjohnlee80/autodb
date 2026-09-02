package frontdoor

import (
	"context"
	"testing"

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
	// reentered records whether emit was able to call back into the engine.
	reentered bool
}

func (q *fakeQueries) WireQuery(_ context.Context, _ exec.SessionID, _ int64, sql, _ string,
	emit func(exec.WireMessage) error) (byte, error) {
	q.sawSQL = append(q.sawSQL, sql)
	if q.err != nil {
		return 0, q.err
	}
	for _, m := range q.msgs {
		if err := emit(m); err != nil {
			return 0, err
		}
	}
	return q.status, nil
}

func (q *fakeQueries) WireTxStatus(_ exec.SessionID, _ int64) (byte, error) {
	if q.txErr != nil {
		return 0, q.txErr
	}
	return q.txStatus, nil
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
	if len(q.sawSQL) != 1 || q.sawSQL[0] != "SELECT 1" {
		t.Fatalf("the engine saw %v, want exactly [SELECT 1] — the loop must pass the buffer through unaltered", q.sawSQL)
	}
}

// The status byte is the ENGINE's, not a constant. A loop that hard-coded 'I'
// would pass the cell above and still tell a client inside a transaction that
// it had nothing to commit.
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

	// THE LOAD-BEARING HALF: the same connection still answers a query.
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the connection did not survive the refusal: %v", err)
	}
	if got := readUntilReady(t, fe); got != txStatusIdle {
		t.Fatalf("readiness after the surviving refusal = %q", got)
	}
	if len(q.sawSQL) != 1 {
		t.Fatalf("the engine saw %v; the query after the refusal did not reach it", q.sawSQL)
	}
}

// A COPY sub-protocol frame is a violation: fatal, and the connection closes.
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
