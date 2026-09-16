package frontdoor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A DUPLICATE NAME IS REJECTED HERE AND ANSWERED WITH POSTGRESQL'S OWN CODE.
//
// The name is claimed against this side's object graph before the Parse is
// forwarded, so the backend never sees the second Parse and there is no target
// error to relay: the SQLSTATE is fixed by the register rather than passed
// through. It must still be 42P05, because that is what a driver's recovery is
// written against -- and until the register existed this condition fell through
// the catalogue's default and answered 42501, telling every client to go and
// check its grants for a name it had simply already used.
//
// The frames after the refusal must be DISCARDED through the client's own Sync,
// exactly as PostgreSQL discards them, and exactly one ReadyForQuery must close
// the segment. A second readiness byte would tell a pipelining client that one
// of its later frames had its own cycle.
func TestPGHeldObjects_ADuplicateNamedParseIsTheFixed42P05(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)
	defer func() { _ = conn.Close() }()

	const name = "autodb_l5_dup"
	fe.Send(&pgproto3.Parse{Name: name, Query: "SELECT 1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	// BASELINE FIRST. Without it a second Parse answered 42P05 proves nothing
	// about duplicate rejection: a first Parse that had itself failed would
	// produce the same shape for a different reason.
	if err := readToReady(t, fe, "the first Parse"); err != nil {
		t.Fatalf("the first Parse was refused, so this cell never reached its subject: %v", err)
	}

	// The duplicate, with a frame pipelined behind it that must never run.
	fe.Send(&pgproto3.Parse{Name: name, Query: "SELECT 1"})
	fe.Send(&pgproto3.Parse{Name: name + "_behind", Query: "SELECT 2"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var refusal *pgproto3.ErrorResponse
	var parseCompletes, readies int
	for readies == 0 {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("receiving the duplicate's answer: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			refusal = m
		case *pgproto3.ParseComplete:
			parseCompletes++
		case *pgproto3.ReadyForQuery:
			readies++
		}
	}

	row, known := heldObjectRowFor(condDuplicateStatement)
	if !known {
		t.Fatal("the duplicate-statement condition has no register row")
	}
	// THE CODE IS WRITTEN OUT HERE AS WELL AS READ FROM THE ROW. Comparing the
	// wire only against the register would make this cell green for any code at
	// all, including the 42501 it used to answer: the register and the wire
	// would simply agree about the wrong thing.
	if row.sqlState != "42P05" {
		t.Fatalf("the register answers a duplicate name with %q; PostgreSQL's own code for "+
			"this condition is 42P05 and a driver's recovery is written against it",
			row.sqlState)
	}
	switch {
	case refusal == nil:
		t.Fatal("a duplicate named Parse was accepted; no ErrorResponse arrived")
	case refusal.Code != row.sqlState:
		t.Errorf("SQLSTATE = %q, want %q", refusal.Code, row.sqlState)
	}
	if refusal != nil {
		if refusal.Severity != row.severity || refusal.SeverityUnlocalized != row.severity {
			t.Errorf("severity = %q/%q, want %q",
				refusal.Severity, refusal.SeverityUnlocalized, row.severity)
		}
		if refusal.Message != row.message {
			t.Errorf("message = %q, want the register's literal %q", refusal.Message, row.message)
		}
		if refusal.Detail != row.identity {
			t.Errorf("DETAIL = %q, want the rule id %q", refusal.Detail, row.identity)
		}
		if refusal.Hint != row.hint {
			t.Errorf("HINT = %q, want %q", refusal.Hint, row.hint)
		}
	}
	// THE FRAME BEHIND IT NEVER RAN. The client pipelined it believing the
	// duplicate would succeed; acting on it would run work against a premise
	// that is now false.
	if parseCompletes != 0 {
		t.Errorf("%d ParseComplete(s) arrived after the refusal; the segment did not discard",
			parseCompletes)
	}
	if readies != 1 {
		t.Errorf("%d ReadyForQuery bytes closed the segment, want exactly 1", readies)
	}

	// THE AUDIT TRAIL RECORDS THE REGISTER'S IDENTITY. The peer's DETAIL and
	// the operator's grep must land on the same row, which is the whole reason
	// one string serves both.
	var audited bool
	for _, event := range l.events() {
		if event.Reason == row.identity {
			audited = true
		}
	}
	if !audited {
		t.Errorf("no event carried %q; the operator cannot find this refusal", row.identity)
	}

	// AND THE SESSION IS STILL USABLE. A refusal that keeps the session must
	// keep it: the register says the connection survives, and a cell that
	// stopped at the SQLSTATE would be green for a refusal that quietly closed.
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := readToReady(t, fe, "the statement after the refusal"); err != nil {
		t.Fatalf("the session did not survive its own refusal: %v", err)
	}
}

// THE SAME CONDITION, THROUGH A REAL DRIVER, WITH THE RECOVERY THE DRIVER
// ACTUALLY PERFORMS.
//
// Hand-built frames prove the shape and nothing about whether a client can act
// on it. pgx must see a PgError carrying 42P05, keep the connection, and run
// the next statement on it -- which is the whole difference between a refusal
// and a session a driver throws away.
func TestPGHeldObjects_PgxRecoversFromTheDuplicateRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := driverCfg(t, pgx.QueryExecModeCacheStatement)
	conn, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	const name = "autodb_l5_pgx_dup"
	if _, perr := conn.Prepare(ctx, name, "SELECT 1 AS n", nil); perr != nil {
		t.Fatalf("the first Prepare failed, so this cell never reached its subject: %v", perr)
	}

	_, perr := conn.Prepare(ctx, name, "SELECT 1 AS n", nil)
	if perr == nil {
		t.Fatal("re-preparing a live name succeeded; the duplicate was not rejected")
	}
	var pgErr *pgconn.PgError
	if !errors.As(perr, &pgErr) {
		t.Fatalf("the driver received %v, which is not a PostgreSQL error it can branch on", perr)
	}
	row, _ := heldObjectRowFor(condDuplicateStatement)
	if pgErr.Code != row.sqlState {
		t.Errorf("pgx saw SQLSTATE %q, want %q — a driver branches on this code, and a "+
			"wrong branch is a wrong recovery", pgErr.Code, row.sqlState)
	}
	if pgErr.Message != row.message {
		t.Errorf("pgx saw message %q, want the register's literal %q", pgErr.Message, row.message)
	}

	// THE RECOVERY ITSELF: the same connection, still working.
	if _, rerr := conn.Exec(ctx, "SELECT 1").ReadAll(); rerr != nil {
		t.Fatalf("the connection did not survive the rejection: %v", rerr)
	}

	// And the name is still the FIRST statement's, untouched by the rejected
	// one. A rejection that mutated the live object would be worse than one
	// that relayed a duplicate: the client would hold a handle to something
	// that had silently changed.
	run := conn.ExecPrepared(ctx, name, nil, nil, nil).Read()
	if run.Err != nil {
		t.Fatalf("the statement that already existed no longer runs: %v", run.Err)
	}
	if len(run.Rows) != 1 {
		t.Fatalf("the surviving statement returned %d rows, want 1", len(run.Rows))
	}
}

// readToReady drains one segment and reports the first ErrorResponse in it.
func readToReady(t *testing.T, fe *pgproto3.Frontend, what string) error {
	t.Helper()
	var failure error
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("receiving %s: %v", what, err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			failure = &pgconn.PgError{Code: m.Code, Message: m.Message, Severity: m.Severity}
		case *pgproto3.ReadyForQuery:
			return failure
		}
	}
}

// A DUPLICATE PORTAL IS 42P03 AGAINST A REAL SERVER, AND THE DRIVER RECOVERS.
//
// This is the condition the partial register got wrong in the way that is
// hardest to see: a duplicate STATEMENT answered 42P05 and a duplicate PORTAL,
// which had no row, fell through the catalogue's default to 42501 -- so one
// client meeting two halves of the same mistake was told to check its grants
// for one of them. 42P03 duplicate_cursor is what a real server answers, and a
// driver that branches on it closes the portal rather than asking an
// administrator for privileges it already has.
//
// BOTH BINDS ARE IN ONE SEGMENT, deliberately: a portal's lifetime ends with
// the transaction the segment implies, so binding twice across a Sync would be
// asking a different question and would pass for the wrong reason.
func TestPGHeldObjects_ADuplicatePortalIs42P03AndPgxRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := pgxThroughFrontDoor(t, ctx)
	fe := conn.Frontend()

	fe.Send(&pgproto3.Parse{Name: "autodb_dup_portal_s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "autodb_dup_portal_p", PreparedStatement: "autodb_dup_portal_s"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "autodb_dup_portal_p", PreparedStatement: "autodb_dup_portal_s"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the duplicate Bind: %v", err)
	}

	got, binds := readSegment(t, fe)
	// BASELINE INSIDE THE SEGMENT. Without the first BindComplete a 42P03
	// proves nothing about duplication: a first Bind that had itself failed
	// would produce the same shape for a different reason.
	if binds != 1 {
		t.Fatalf("%d BindCompletes arrived; the first Bind must succeed or this cell never "+
			"reached its subject", binds)
	}
	row, known := heldObjectRowFor(condDuplicatePortal)
	if !known {
		t.Fatal("the duplicate-portal condition has no register row")
	}
	// WRITTEN OUT AS WELL AS READ FROM THE ROW, so the register and the wire
	// cannot simply agree about the wrong code.
	if row.sqlState != "42P03" {
		t.Fatalf("the register answers a duplicate portal with %q, and a real server answers "+
			"42P03", row.sqlState)
	}
	assertLiveRefusal(t, got, row)
	assertPgxStillWorks(t, ctx, conn)
}

// AN EXECUTE NAMING A PORTAL THAT DOES NOT EXIST IS 34000, AND THE DRIVER
// RECOVERS.
//
// The portal half of the same pair as 26000. Without a row it fell through to
// 42501 and told the client it lacked privileges for a portal it had simply
// never bound -- a recovery it cannot perform, on a connection that was fine.
func TestPGHeldObjects_AnUnknownPortalIs34000AndPgxRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := pgxThroughFrontDoor(t, ctx)
	fe := conn.Frontend()

	fe.Send(&pgproto3.Execute{Portal: "autodb_never_bound"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the Execute: %v", err)
	}

	got, _ := readSegment(t, fe)
	row, known := heldObjectRowFor(condUnknownPortal)
	if !known {
		t.Fatal("the unknown-portal condition has no register row")
	}
	if row.sqlState != "34000" {
		t.Fatalf("the register answers an unknown portal with %q, and a real server answers "+
			"34000", row.sqlState)
	}
	assertLiveRefusal(t, got, row)
	assertPgxStillWorks(t, ctx, conn)
}

// A BIND NAMING A STATEMENT THAT DOES NOT EXIST IS 26000 THROUGH THE DRIVER'S
// OWN API, AND THE DRIVER RECOVERS.
//
// ExecPrepared sends exactly the Bind/Execute/Sync a driver sends when it
// believes it holds a prepared statement, which is the real shape of this
// condition: a statement destroyed by an aborted segment, named afterwards by a
// client whose cache has not caught up. It must answer what a real server
// answers, because the recovery -- parse it again and retry -- is only reachable
// from 26000.
func TestPGHeldObjects_AnUnknownStatementIs26000AndPgxRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := pgxThroughFrontDoor(t, ctx)

	res := conn.ExecPrepared(ctx, "autodb_never_prepared", nil, nil, nil).Read()
	if res.Err == nil {
		t.Fatal("running a statement that was never prepared succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(res.Err, &pgErr) {
		t.Fatalf("the driver received %v, which is not a PostgreSQL error it can branch on",
			res.Err)
	}
	row, known := heldObjectRowFor(condUnknownStatement)
	if !known {
		t.Fatal("the unknown-statement condition has no register row")
	}
	if row.sqlState != "26000" {
		t.Fatalf("the register answers an unknown statement with %q, and a real server answers "+
			"26000", row.sqlState)
	}
	if pgErr.Code != row.sqlState {
		t.Errorf("pgx saw SQLSTATE %q, want %q", pgErr.Code, row.sqlState)
	}
	if pgErr.Message != row.message {
		t.Errorf("pgx saw message %q, want the register's literal %q", pgErr.Message, row.message)
	}
	if strings.Contains(pgErr.Message, "exec:") {
		t.Errorf("the engine's own error text reached the driver: %q", pgErr.Message)
	}
	assertPgxStillWorks(t, ctx, conn)
}

// pgxThroughFrontDoor opens a real pgx connection to the live front door and
// proves it works before the cell does anything unusual with it.
func pgxThroughFrontDoor(t *testing.T, ctx context.Context) *pgconn.PgConn {
	t.Helper()

	cfg := driverCfg(t, pgx.QueryExecModeCacheStatement)
	conn, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "SELECT 1").ReadAll(); err != nil {
		t.Fatalf("the baseline query failed, so this cell never reached its subject: %v", err)
	}
	return conn
}

// readSegment drains one extended segment and reports its refusal, plus how
// many Binds completed before it.
func readSegment(t *testing.T, fe *pgproto3.Frontend) (*pgproto3.ErrorResponse, int) {
	t.Helper()

	var refusal *pgproto3.ErrorResponse
	binds := 0
	for range 64 {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			refusal = m
		case *pgproto3.BindComplete:
			binds++
		case *pgproto3.ReadyForQuery:
			return refusal, binds
		}
	}
	t.Fatal("the segment never ended with a readiness byte")
	return nil, 0
}

// assertLiveRefusal compares a frame that came back over a real connection with
// the register row that produced it.
func assertLiveRefusal(t *testing.T, got *pgproto3.ErrorResponse, row heldObjectRow) {
	t.Helper()

	if got == nil {
		t.Fatalf("no ErrorResponse arrived; %q was not refused at all", row.identity)
	}
	if got.Code != row.sqlState {
		t.Errorf("SQLSTATE = %q, want %q", got.Code, row.sqlState)
	}
	if got.Severity != row.severity || got.SeverityUnlocalized != row.severity {
		t.Errorf("severity = %q/%q, want %q", got.Severity, got.SeverityUnlocalized, row.severity)
	}
	if got.Message != row.message {
		t.Errorf("message = %q, want the register's literal %q", got.Message, row.message)
	}
	if got.Detail != row.identity {
		t.Errorf("DETAIL = %q, want the rule id %q", got.Detail, row.identity)
	}
	if got.Hint != row.hint {
		t.Errorf("HINT = %q, want %q", got.Hint, row.hint)
	}
	for _, field := range []string{got.Message, got.Hint, got.Detail} {
		for _, bad := range forbiddenOnTheWire {
			if strings.Contains(field, bad) {
				t.Errorf("the peer received %q, which carries the internal term %q", field, bad)
			}
		}
	}
}

// assertPgxStillWorks is the recovery half: a refusal keeps the session, and a
// cell that stopped at the SQLSTATE would be green for one that quietly closed.
func assertPgxStillWorks(t *testing.T, ctx context.Context, conn *pgconn.PgConn) {
	t.Helper()

	if _, err := conn.Exec(ctx, "SELECT 1").ReadAll(); err != nil {
		t.Fatalf("the driver's connection did not survive the refusal: %v", err)
	}
}
