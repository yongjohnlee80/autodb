package frontdoor

import (
	"context"
	"fmt"
	"net"
	"strings"
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

// noExtended satisfies the extended half of QueryExecutor for doubles whose cell
// is about the SIMPLE path. Embedding it keeps those cells from carrying nine
// methods they never call, and it is a compile-time reminder that the seam grew.
type noExtended struct{}

func (noExtended) WireParse(context.Context, exec.SessionID, int64, string, string, []uint32, string) error {
	return nil
}
func (noExtended) WireBind(context.Context, exec.SessionID, int64, string, string, [][]byte, []int16, []int16) error {
	return nil
}
func (noExtended) WireDescribeStatement(context.Context, exec.SessionID, int64, string) error {
	return nil
}
func (noExtended) WireDescribePortal(context.Context, exec.SessionID, int64, string) error {
	return nil
}
func (noExtended) WireCloseStatement(context.Context, exec.SessionID, int64, string) error {
	return nil
}
func (noExtended) WireClosePortal(context.Context, exec.SessionID, int64, string) error { return nil }
func (noExtended) WireExecutePortal(context.Context, exec.SessionID, int64, string, uint32, string, func(exec.WireMessage) error) error {
	return nil
}
func (noExtended) WireFlushSegment(context.Context, exec.SessionID, int64, func(exec.WireMessage) error) error {
	return nil
}
func (noExtended) WireSyncSegment(context.Context, exec.SessionID, int64) (byte, error) {
	return txStatusIdle, nil
}

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
	// stop, when set, is the report the engine wraps an emitter failure in —
	// exactly as core/exec does. A fake that returned the consumer's error BARE
	// would leave errors.As finding nothing, so every post-dispatch cell would
	// silently exercise the unresolved arm and the other five would be untested
	// while looking covered. Cause is filled in at the point of failure.
	stop *exec.EmitStopped
	// txStatus is what WireTxStatus reports (the readiness after a refusal).
	txStatus byte
	// txErr, when set, fails the status read.
	txErr error

	// sawSQL records every statement the loop passed down.
	sawSQL []string

	// The extended surface: what the loop asked for, what the engine emits back,
	// and the two refusals a cell may want to script.
	extCalls   []string
	extMsgs    []exec.WireMessage
	parseErr   error
	executeErr error
	syncErr    error
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
			q.mu.Lock()
			stop := q.stop
			q.mu.Unlock()
			if stop == nil {
				return 0, eerr
			}
			// A copy, so a shared template is not mutated across calls.
			wrapped := *stop
			wrapped.Cause = eerr
			return wrapped.TxStatus, &wrapped
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

// THE EXTENDED SURFACE. Scripted the same way the simple one is: the cells say
// what the engine emits and this records what the loop asked for, so a cell can
// assert the ROUTING without a database.
func (q *fakeQueries) extRecord(call string) {
	q.mu.Lock()
	q.extCalls = append(q.extCalls, call)
	q.mu.Unlock()
}

func (q *fakeQueries) calls() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.extCalls...)
}

func (q *fakeQueries) WireParse(_ context.Context, _ exec.SessionID, _ int64, name, sqlText string, _ []uint32, _ string) error {
	q.extRecord("Parse:" + name + ":" + sqlText)
	return q.parseErr
}

func (q *fakeQueries) WireBind(_ context.Context, _ exec.SessionID, _ int64, portal, stmt string, _ [][]byte, _, _ []int16) error {
	q.extRecord("Bind:" + portal + ":" + stmt)
	return nil
}

func (q *fakeQueries) WireDescribeStatement(_ context.Context, _ exec.SessionID, _ int64, name string) error {
	q.extRecord("DescribeS:" + name)
	return nil
}

func (q *fakeQueries) WireDescribePortal(_ context.Context, _ exec.SessionID, _ int64, name string) error {
	q.extRecord("DescribeP:" + name)
	return nil
}

func (q *fakeQueries) WireCloseStatement(_ context.Context, _ exec.SessionID, _ int64, name string) error {
	q.extRecord("CloseS:" + name)
	return nil
}

func (q *fakeQueries) WireClosePortal(_ context.Context, _ exec.SessionID, _ int64, name string) error {
	q.extRecord("CloseP:" + name)
	return nil
}

func (q *fakeQueries) WireExecutePortal(_ context.Context, _ exec.SessionID, _ int64,
	portal string, _ uint32, _ string, emit func(exec.WireMessage) error) error {
	q.extRecord("Execute:" + portal)
	if q.executeErr != nil {
		return q.executeErr
	}
	q.mu.Lock()
	msgs := q.extMsgs
	q.mu.Unlock()
	for _, m := range msgs {
		if err := emit(m); err != nil {
			return err
		}
	}
	return nil
}

func (q *fakeQueries) WireFlushSegment(_ context.Context, _ exec.SessionID, _ int64,
	emit func(exec.WireMessage) error) error {
	q.extRecord("Flush")
	q.mu.Lock()
	msgs := q.extMsgs
	q.mu.Unlock()
	for _, m := range msgs {
		if err := emit(m); err != nil {
			return err
		}
	}
	return nil
}

func (q *fakeQueries) WireSyncSegment(_ context.Context, _ exec.SessionID, _ int64) (byte, error) {
	q.extRecord("Sync")
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.syncErr != nil {
		return 0, q.syncErr
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
// A WHOLE SEGMENT IN ONE FLUSH — Parse, Bind, Execute, Sync — which is how every
// extended client sends one.
//
// This is F2's first witness and it is RED until ultron-prime's pipelining fix
// lands: the loop cannot currently read past the first frame of a pipelined
// write. It is committed red deliberately rather than written to send one frame
// at a time, because a cell that avoids pipelining would pass and mean nothing —
// no real client sends that way.
//
// Witness for rows 4:Parse, 4:Bind, 4:Execute and 4:Sync at the wire: the client
// drives a whole segment and the engine sees each frame in order, with the
// readiness byte at the end coming from the engine's Sync rather than being
// synthesised by the loop.
func TestLoop_ExtendedSegmentReachesTheEngineAndSyncEndsIt(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{
		{Kind: "ParseComplete"},
		{Kind: "BindComplete"},
		{Kind: "DataRow", Values: [][]byte{[]byte("1")}},
		{Kind: "CommandComplete", Tag: "SELECT 1"},
	}
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"})
	fe.Send(&pgproto3.Execute{Portal: "p"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// BOUNDED, because the failure this cell currently observes is a HANG, and a
	// cell that hangs reports nothing.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness for the segment (got %q); the engine saw %v.\n"+
			"If it saw only the FIRST frame this is the PIPELINING defect, not F2 routing: pgproto3's Backend "+
			"wraps the loop's bufio.Reader in its own read-ahead chunkReader, so frames after the first sit "+
			"inside pgproto3 and br.Peek(1) blocks on an empty reader. Owner: ultron-prime, own PR ahead of #57.",
			got, q.calls())
	}
	want := []string{"Parse:s:SELECT 1", "Bind:p:s", "Execute:p", "Sync"}
	got := q.calls()
	if len(got) != len(want) {
		t.Fatalf("the engine saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the engine saw %v, want %v", got, want)
		}
	}
}

// Witness for row 5:CopyInResponse.
//
// A §5 CANARY arriving is a CLASSIFIER BYPASS, and the audit must say so.
//
// This cell previously asserted ruleUnframeableMessage — the identity for a
// message the front door has no case for — and passing was what made the
// conflation look correct. The two are different events: an unframeable message
// is our mapper being incomplete, while a canary means something reached the
// target that classification was supposed to refuse. Recording the second as the
// first sends an operator to debug the front door while the security-relevant
// event goes unnamed. Wire identity stays the catalogue's §7 violation id; the
// AUDIT carries the defect (§1.2).
func TestLoop_ACanaryIsAuditedAsAClassifierBypassAndCloses(t *testing.T) {
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
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok || e.Severity != "FATAL" {
		t.Fatalf("frame = %T (%v), want a FATAL ErrorResponse", msg, msg)
	}
	if e.Code != sqlStateProtocolViolation {
		t.Errorf("wire code = %q, want the catalogue's %q", e.Code, sqlStateProtocolViolation)
	}
	waitFor(t, "the bypass to be audited as a bypass", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == ruleClassifierBypass
	})
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == ruleUnframeableMessage {
			t.Fatal("a classifier bypass was audited as an unframeable message; that records a gate bypass " +
				"as our mapper being incomplete and leaves the real event unnamed")
		}
	}
}

// ...and the OTHER identity still has a witness: a kind the front door genuinely
// has no case for is a front-door defect, and must not be dressed up as a bypass.
//
// Without this cell the split above could be satisfied by auditing everything as
// a bypass, which would be the same conflation pointing the other way.
func TestLoop_AnUnknownMessageKindIsAFrontDoorDefect(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.msgs = []exec.WireMessage{{Kind: "NoSuchBackendMessage"}}
	events, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("reading the failure: %v", err)
	}
	waitFor(t, "the defect to be audited under its own cause", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == ruleUnframeableMessage
	})
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == ruleClassifierBypass {
			t.Fatal("a kind we simply do not handle was audited as a classifier bypass")
		}
	}
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
	noExtended

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

// The second frame of a pipelined pair must REACH THE ENGINE, not sit in
// pgproto3's buffer unseen.
//
// Ultron's cell (PR #59) proves this property with F1 vocabulary, where the
// second frame is a Parse the decision table REFUSES. On this branch F2 routes
// Parse to the engine instead, so the same property needs the same shape with
// the new destination: a Query answered normally, and a Parse behind it in the
// SAME flush that must still be SEEN. Kept as a distinct cell from the segment
// witness because it is the MIXED case — one simple frame and one extended frame
// in one write — which is exactly what lib/pq emits and what neither a
// simple-only nor an extended-only cell covers.
func TestLoop_PipelinedQueryThenParseBothReachTheEngine(t *testing.T) {
	t.Parallel()
	q := okQueries()
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	fe.Send(&pgproto3.Parse{Name: "p2", Query: "SELECT 2"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	// The Query's own readiness, then the segment's.
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness for the pipelined Query (got %q); engine saw sql=%v ext=%v", got, q.statements(), q.calls())
	}
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness for the pipelined Sync (got %q); engine saw sql=%v ext=%v", got, q.statements(), q.calls())
	}
	// Snapshot under the mutex; sawSQL is written on the server goroutine.
	if ran := q.statements(); len(ran) != 1 || ran[0] != "SELECT 1" {
		t.Errorf("the simple half reached the engine as %v, want [SELECT 1]", ran)
	}
	want := []string{"Parse:p2:SELECT 2", "Sync"}
	got := q.calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the extended half reached the engine as %v, want %v — the frame behind the Query was not seen", got, want)
	}
}

// readUntilReadySoft is readUntilReady that RETURNS on a read failure instead of
// failing the cell, so a caller can report what the engine saw alongside it.
func readUntilReadySoft(t *testing.T, fe *pgproto3.Frontend) byte {
	t.Helper()
	for {
		msg, err := fe.Receive()
		if err != nil {
			return 0
		}
		if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return rfq.TxStatus
		}
	}
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

// Matrix row 3.1:application_name#session-audit — the front door's half: the
// ACCEPTED label must reach the ENGINE, not merely the echo.
//
// The distinction is the whole claim. An echo synthesized in the front door from
// the startup params would satisfy a cell that only reads the wire, while the
// session recorded nothing and every audit row said app "". So this asserts what
// OpenWireSessionWith was CALLED with, and asserts the echo agrees with it —
// the engine's half (recorded on the session and on every audit line) is proven
// by core/exec's TestWireOpen_ApplicationNameIsOnTheSessionAndEveryAuditLine and
// TestWireOpen_AuditStampCoversDecodedAndOwnedControlSites.
func TestLoop_TheAcceptedApplicationNameReachesTheEngine(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	const label = "reporting-tool-7"
	conn, fe := startupTo(t, addr, map[string]string{
		"user": "root", "database": "target", "application_name": label,
	})
	defer func() { _ = conn.Close() }()
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	echoed := ""
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if ps, ok := msg.(*pgproto3.ParameterStatus); ok && ps.Name == "application_name" {
			echoed = ps.Value
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	got := f.openedAppNames()
	if len(got) != 1 || got[0] != label {
		t.Fatalf("the engine was opened with %v, want exactly [%q] — the label must reach the "+
			"SESSION, and a front door that only echoes it leaves every audit row saying app \"\"",
			got, label)
	}
	if echoed != label {
		t.Fatalf("echoed %q, want %q — the echo comes from what the engine ACCEPTED, so it "+
			"cannot disagree with what the session records", echoed, label)
	}
}

// EVERY ARM, THROUGH THE LOOP. The engine decides which arm; this asserts the
// loop's story for each one and that no two arms tell the same story.
//
// It is table-driven against the fake rather than live PG because the arms are
// the ENGINE's vocabulary and only some are reachable by arranging a real
// database — ArmAborted in particular. The live-PG cells prove which arm each
// real path produces; these prove what the loop says once it has one. Both are
// needed, and neither substitutes for the other.
func TestLoop_EveryEmitStoppedArmHasItsOwnStory(t *testing.T) {
	t.Parallel()
	cap := int64(64) // small enough that the first frame trips it

	for _, arm := range []struct {
		name      string
		stop      *exec.EmitStopped
		wantIn    string // must appear in what the client is told
		wantOut   string // the audit's effects= word
		forbid    string // must NOT appear — the story this arm is confused with
		wantReady byte   // the readiness byte that must follow, or 0 to not check
	}{
		{"no statement", &exec.EmitStopped{Executed: false, TxStatus: exec.TxStatusIdle},
			"nothing ran", "no_statement", "effects are committed", txStatusIdle},
		{"failed at the target", &exec.EmitStopped{Executed: true, TxStatus: exec.TxStatusIdle, TargetErr: &pgconn.PgError{Code: "22012"}},
			"failed at the target", "failed", "effects are committed", txStatusIdle},
		{"pending inside a transaction", &exec.EmitStopped{Executed: true, TxStatus: exec.TxStatusInTx},
			"PENDING", "pending_commit", "effects are committed", txStatusInTx},
		{"aborted transaction", &exec.EmitStopped{Executed: true, TxStatus: exec.TxStatusAborted},
			"aborted", "aborted", "effects are committed", txStatusAborted},
		{"completed", &exec.EmitStopped{Executed: true, TxStatus: exec.TxStatusIdle, Outcome: exec.StatusOK},
			"effects are committed", "completed", "not known", txStatusIdle},
		{"unresolved", &exec.EmitStopped{Executed: true, TxStatus: exec.TxStatusIdle},
			"not known", "unresolvable", "effects are committed", txStatusIdle},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			q := okQueries()
			q.stop = arm.stop
			q.msgs = []exec.WireMessage{{Kind: "DataRow", Values: [][]byte{make([]byte, 256)}}}
			events, addr := listenerForArms(t, q, &cap)
			conn, fe := authenticated(t, addr)
			defer func() { _ = conn.Close() }()

			fe.Send(&pgproto3.Query{String: "SELECT 1"})
			if err := fe.Flush(); err != nil {
				t.Fatal(err)
			}
			var told *pgproto3.ErrorResponse
			for told == nil {
				msg, err := fe.Receive()
				if err != nil {
					t.Fatalf("reading the arm's report: %v", err)
				}
				if e, ok := msg.(*pgproto3.ErrorResponse); ok {
					told = e
				}
			}
			said := told.Message + " " + told.Hint
			if !strings.Contains(said, arm.wantIn) {
				t.Fatalf("the client was told %q, want it to contain %q", said, arm.wantIn)
			}
			if arm.forbid != "" && strings.Contains(said, arm.forbid) {
				t.Fatalf("the %s arm told the client %q, which is another arm's story", arm.name, arm.forbid)
			}
			if !hasEventDetail(events(), "fd.stmt_outcome", "effects="+arm.wantOut) {
				t.Fatalf("audit must record effects=%s; events=%v", arm.wantOut, events())
			}

			// AND THE READINESS BYTE MUST AGREE WITH THE STORY (r0 MF1). Reading
			// only the ErrorResponse let this table PASS while telling the client
			// its effects were PENDING and then handing it readiness `I` in the
			// same cycle — the fixture supplies stopped.TxStatus=T while the
			// fake's WireTxStatus answers I, so the contradiction was constructed
			// here and never looked at.
			rfq, ok := firstOfType[*pgproto3.ReadyForQuery](drainToReady(t, conn, fe))
			if !ok {
				t.Fatal("no readiness followed the report; a session-surviving stop owes one")
			}
			if arm.wantReady != 0 && rfq.TxStatus != arm.wantReady {
				t.Fatalf("the client was told %q and then handed readiness %q — one answer, two "+
					"halves, and they disagree about the transaction", said, rfq.TxStatus)
			}
		})
	}
}

// listenerForArms starts a loop listener with a lowered output cap.
func listenerForArms(t *testing.T, q QueryExecutor, cap *int64) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q,
		AuthFailuresPerIP: unthrottled, testOutputCap: cap,
	})
	return events, addr
}

// drainToReady reads frames until the cycle's ReadyForQuery, so a cell can
// assert the readiness byte rather than stopping at the error that precedes it.
func drainToReady(t *testing.T, conn net.Conn, fe *pgproto3.Frontend) []pgproto3.BackendMessage {
	t.Helper()
	// BOUNDED (r1 residual 2). Without a deadline, a mutation that deletes the
	// readiness byte makes this cell HANG rather than fail — and a cell that
	// hangs reports nothing, which is the failure mode it was added to catch.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("bounding the readiness drain: %v", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var out []pgproto3.BackendMessage
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("draining to readiness after %d frames: %v — a missing readiness byte must "+
				"fail here promptly, not hang", len(out), err)
		}
		out = append(out, msg)
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return out
		}
	}
}

// An engine report whose transaction status is not a status leaves the session's
// phase UNKNOWN, and §6.3 withholds readiness for exactly that. It must not be
// repaired from a later snapshot: that is the split this PR removed, returning
// in the one case where the engine's own answer is already suspect.
func TestLoop_AnInvalidEngineReportWithholdsReadiness(t *testing.T) {
	t.Parallel()
	cap := int64(64)
	q := okQueries()
	q.stop = &exec.EmitStopped{Executed: true, TxStatus: 'Z'} // not I, T or E
	q.txStatus = txStatusIdle                                 // a later read WOULD say idle
	q.msgs = []exec.WireMessage{{Kind: "DataRow", Values: [][]byte{make([]byte, 256)}}}
	_, addr := listenerForArms(t, q, &cap)

	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			return // the connection ended without readiness, which is the rule
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			t.Fatal("readiness was sent for a session whose phase the engine could not state — " +
				"the byte was repaired from a later snapshot, which is the split this PR removed")
		}
	}
}
