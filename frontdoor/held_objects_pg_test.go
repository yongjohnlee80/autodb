package frontdoor

import (
	"context"
	"errors"
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
