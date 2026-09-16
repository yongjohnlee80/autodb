package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE HALF THAT DECIDES THE SQLSTATE: A REAL DRIVER, A REAL TARGET, AND A
// SESSION THAT IS STILL USABLE AFTERWARDS.
//
// The unit cells in dial_failed_test.go prove the frame is what this package
// says it is. They cannot prove the thing that actually matters, which is what
// a driver DOES with it — and the choice of code turns entirely on that. Class
// 08 is the obvious code for "the connection failed" and many drivers mark the
// frontend connection broken on it, which would convert "this request failed"
// into "your session died" without a single frame being wrong.
//
// So these cells run real pgx against a real front door in front of a real
// PostgreSQL target: the failure is produced, the error is read out of the
// driver's own error type, and then THE SAME CONNECTION does real work against
// the real target. A cell that stopped at the error would prove nothing about
// survival.
//
// THE JDBC HALF HAS NOT BEEN RUN. It is a manual procedure, written out in
// docs/front-door/dial-failed-client-verification.md, and until it passes the
// registered shape rests on one client of the two.

// dialFaultQueries is the real engine with ONE injectable condition: the
// backend for a request could not be acquired.
//
// INJECTED AT THIS SEAM RATHER THAN BY BREAKING A TARGET, and the reason is
// that a target broken from the outside fails a session at OPEN — today the
// backend is pinned when the session is admitted, so an unreachable target
// never produces the mid-session condition these cells are about. Injecting the
// condition here leaves every other thing real: the classification, the frame,
// the discard through Sync, the readiness byte, the audit event, the socket, the
// driver, and the target the session goes on to use.
type dialFaultQueries struct {
	*exec.Engine
}

// dialFaultMarker arms the fault FROM THE STATEMENT TEXT rather than from a
// flag the cell flips.
//
// A flag would have to be flipped between the failing statement and the one
// that proves the session survived, and the cells that matter most are the ones
// driving an EXTERNAL client — a separate process, which cannot reach into this
// one to flip anything. Marking the statement lets the same fixture serve a Go
// cell and a client written in another language, and it keeps the two halves of
// every cell — the failure and the recovery — in one uninterrupted client
// conversation.
const dialFaultMarker = "/*dial-fault*/"

func dialFaultArmed(sqlText string) bool { return strings.Contains(sqlText, dialFaultMarker) }

// dialFaultCause names a host and a port, so a cell can prove neither reaches
// the client.
func dialFaultCause() error {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 6543},
		Err:  errors.New("connect: connection refused"),
	}
}

func (q *dialFaultQueries) WireQuery(ctx context.Context, id exec.SessionID, userID int64,
	sql, ip string, emit func(exec.WireMessage) error) (byte, error) {

	if dialFaultArmed(sql) {
		return 0, exec.NewDialFailure(dialFaultCause())
	}
	return q.Engine.WireQuery(ctx, id, userID, sql, ip, emit)
}

func (q *dialFaultQueries) WireParse(ctx context.Context, id exec.SessionID, userID int64,
	name, sqlText string, paramOIDs []uint32, ip string) error {

	if dialFaultArmed(sqlText) {
		return exec.NewDialFailure(dialFaultCause())
	}
	return q.Engine.WireParse(ctx, id, userID, name, sqlText, paramOIDs, ip)
}

// dialFaultLoop is the live fixture plus a second listener whose engine seam can
// be made to fail an acquisition. Everything else — the store, the credential,
// the connection row, the target — is the fixture's.
func dialFaultLoop(t *testing.T) (q *dialFaultQueries, addr, secret, database string, events func() []Event) {
	t.Helper()
	h := pgLoopFull(t)
	q = &dialFaultQueries{Engine: h.eng}
	_, ev, a := listenerWith(t, Options{
		Authn: h.eng, Queries: q, AuthFailuresPerIP: unthrottled,
	})
	return q, a, h.secret, h.database, ev
}

// assertLivePgError checks the driver's own view of the frame, and that the
// driver did not decide the connection was broken.
func assertLivePgError(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("pgx reported %T (%v), want a *pgconn.PgError — a driver that cannot "+
			"parse this as a server error will treat it as a transport failure", err, err)
	}
	if pgErr.Code != DialFailedSQLState {
		t.Errorf("pgx read SQLSTATE %q, want %q", pgErr.Code, DialFailedSQLState)
	}
	if pgErr.Severity != "ERROR" {
		t.Errorf("pgx read severity %q, want ERROR — FATAL tells the driver the backend "+
			"is closing the connection", pgErr.Severity)
	}
	if pgErr.Message != DialFailedMessage {
		t.Errorf("pgx read message %q, want the fixed literal %q", pgErr.Message, DialFailedMessage)
	}
	if pgErr.Detail != DialFailedRule {
		t.Errorf("pgx read detail %q, want the stable rule id %q", pgErr.Detail, DialFailedRule)
	}
	for _, leak := range []string{"203.0.113.9", "6543", "refused"} {
		for _, v := range []string{pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where,
			pgErr.SchemaName, pgErr.TableName, pgErr.ColumnName, pgErr.ConstraintName,
			pgErr.File, pgErr.Routine, pgErr.InternalQuery} {
			if strings.Contains(v, leak) {
				t.Errorf("the raw dial cause reached pgx: %q carries %q", v, leak)
			}
		}
	}
}

// dialFaultConn opens a real pgconn to the faulted front door.
func dialFaultConn(t *testing.T, ctx context.Context, addr, secret, database string) *pgconn.PgConn {
	t.Helper()
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	cfg, err := pgconn.ParseConfig(fmt.Sprintf("postgres://root:%s@%s:%s/%s?sslmode=require",
		secret, host, port, database))
	if err != nil {
		t.Fatalf("parsing the driver DSN: %v", err)
	}
	cfg.TLSConfig.InsecureSkipVerify = true // the cell's own listener is self-signed
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// SIMPLE PROTOCOL. pgx reads the fixed frame, keeps the connection, and the very
// next statement on the SAME connection returns a real row from the real target.
func TestDialFailedPG_PgxKeepsTheSessionAcrossASimpleQueryFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, addr, secret, database, events := dialFaultLoop(t)
	conn := dialFaultConn(t, ctx, addr, secret, database)

	if _, err := conn.Exec(ctx, "SELECT 1 "+dialFaultMarker).ReadAll(); err == nil {
		t.Fatal("the armed acquisition produced no error")
	} else {
		assertLivePgError(t, err)
	}

	if conn.IsClosed() {
		t.Fatal("pgx closed the connection on the dial-failure code; the session is " +
			"supposed to survive, and a code the driver treats as connection-fatal " +
			"breaks that promise without a single frame being wrong")
	}

	// THE SESSION IS STILL A SESSION. Real work, on the same connection, against
	// the real target.
	res, err := conn.Exec(ctx, "SELECT 40 + 2 AS n").ReadAll()
	if err != nil {
		t.Fatalf("the statement after the dial failure failed: %v", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || string(res[0].Rows[0][0]) != "42" {
		t.Fatalf("the statement after the dial failure returned %+v, want one row of 42", res)
	}

	var sawAudit bool
	for _, ev := range events() {
		if ev.Kind == EventDialFailed && strings.Contains(ev.Detail, "203.0.113.9") {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Error("the operator's trail has no dial-failure event carrying the cause; the " +
			"client is denied it on the understanding that the operator is not")
	}
}

// EXTENDED PROTOCOL. The same promise on the path where recovery is not a
// readiness byte but a discard through the client's own Sync.
func TestDialFailedPG_PgxKeepsTheSessionAcrossAnExtendedFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, addr, secret, database, _ := dialFaultLoop(t)
	conn := dialFaultConn(t, ctx, addr, secret, database)

	failed := conn.ExecParams(ctx, "SELECT $1::int AS n "+dialFaultMarker, [][]byte{[]byte("7")}, nil, nil, nil).Read()
	if failed.Err == nil {
		t.Fatal("the armed acquisition produced no error on the extended path")
	}
	assertLivePgError(t, failed.Err)

	if conn.IsClosed() {
		t.Fatal("pgx closed the connection after an extended-protocol dial failure")
	}

	res := conn.ExecParams(ctx, "SELECT $1::int AS n", [][]byte{[]byte("42")}, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("the extended statement after the dial failure failed: %v", res.Err)
	}
	if len(res.Rows) != 1 || string(res.Rows[0][0]) != "42" {
		t.Fatalf("the extended statement after the dial failure returned %+v, want one row of 42", res.Rows)
	}
}

// A WHOLE PIPELINED SEGMENT, SENT IN ONE WRITE, GETS ONE ERROR AND ONE
// READINESS BYTE — against the live loop, with the real engine ending the
// discard.
//
// Driven with a raw frontend rather than through pgx, because pgx will not send
// a segment it has been told has already failed: the property is about what
// reaches a client that pipelined BEFORE the answer arrived, and only a raw
// frontend can put that on the wire. The Flush in the middle is deliberate — it
// is not a segment boundary, so it must neither end the discard nor produce a
// second readiness byte.
func TestDialFailedPG_APipelinedSegmentGetsOneErrorAndOneReadyForQuery(t *testing.T) {
	_, addr, secret, database, _ := dialFaultLoop(t)
	fe := pgClientAs(t, addr, secret, database, "root")

	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1 " + dialFaultMarker})
	fe.Send(&pgproto3.Bind{PreparedStatement: "s1", DestinationPortal: "p1"})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "p1"})
	fe.Send(&pgproto3.Execute{Portal: "p1"})
	fe.Send(&pgproto3.Flush{})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	errFrames, readies := 0, 0
	var got *pgproto3.ErrorResponse
	for readies == 0 {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answers: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			errFrames++
			got = m
		case *pgproto3.ReadyForQuery:
			readies++
		default:
			t.Errorf("the segment produced a %T; everything between the error and the "+
				"matching Sync must be discarded", m)
		}
	}
	if errFrames != 1 {
		t.Errorf("the segment produced %d ErrorResponses, want exactly 1 — one refusal "+
			"for one segment", errFrames)
	}
	if got == nil {
		t.Fatal("no ErrorResponse")
	}
	assertDialFailedFrame(t, got)

	// AND THE SESSION IS STILL USABLE, on the live target, through the same
	// socket.
	fe.Send(&pgproto3.Query{String: "SELECT 42 AS n"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the live session did not survive the dial failure: %v (so far: %v)", err, kinds)
		}
		kinds = append(kinds, msgKind(msg))
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sameKinds(kinds, []string{"RowDescription", "DataRow", "CommandComplete", "ReadyForQuery"}) {
		t.Fatalf("the statement after the dial failure answered %v, want a whole result "+
			"from the real target", kinds)
	}
}
