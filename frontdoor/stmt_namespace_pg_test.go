package frontdoor

// THE NAMESPACE A WIRE SESSION HANDS TO ITS CLIENT.
//
// A wire session pins ONE target backend and relays the client's Parse onto it.
// So every named object autodb creates on that session sits in the same
// namespace as the client's, and pgx names a cached statement `stmtcache_` +
// sha256(sql)[:24] -- a pure function of the SQL TEXT. Two pgx instances
// therefore agree on the name for a text they both run, without ever having
// met.
//
// That is not hypothetical. autodb verified the session's parsing mode by
// running `SHOW standard_conforming_strings` on the pinned backend, so a client
// that ran the same text -- autodb's own dbase, whose pool checks exactly that
// -- had its FIRST Parse answered with 42P05 "already exists" on a brand-new
// connection. Nothing recovered, because nothing was broken: both sides were
// correctly using the name they had computed.
//
// The fix was not to rename anything. It was to stop issuing the statement:
// standard_conforming_strings is a reported parameter, so the value arrives
// unasked.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A CLIENT MAY PREPARE THE TEXT AUTODB VERIFIES WITH.
//
// The client here is pgx in its SHIPPED DEFAULT -- QueryExecModeCacheStatement,
// which names and caches. Configuring around it would test a client nobody
// deploys, and the default is what broke.
//
// The mutation is restoring the `SHOW standard_conforming_strings` query in
// pgPrepareConnVerify: the first run below then fails with 42P05 before any
// Bind or Execute, which is exactly what production showed.
func TestStmtNamespace_AClientMayPrepareTheTextAutodbVerifiesWith(t *testing.T) {
	const verified = "SHOW standard_conforming_strings"

	cfg, err := pgx.ParseConfig(driverDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	// Twice: the first Parse claims the name, the second must be a cache hit
	// that sends no Parse at all. A fix that merely renamed autodb's statement
	// would pass the first and could still fail the second.
	for i := 1; i <= 2; i++ {
		var scs string
		if err := conn.QueryRow(ctx, verified).Scan(&scs); err != nil {
			t.Fatalf("run %d of %q through the front door: %v", i, verified, err)
		}
		if scs != "on" {
			t.Fatalf("run %d returned %q, want \"on\"", i, scs)
		}
	}
}

// AND THE SESSION STILL CARRIES NO AUTODB-OWNED PREPARED STATEMENT.
//
// The cell above would also pass if autodb kept preparing its statement under
// some OTHER name -- which would leave the same defect waiting for the next
// text a client happens to share. This one asserts the namespace itself: after
// a session is open and has served a statement, pg_prepared_statements holds
// exactly what the CLIENT put there and nothing else.
func TestStmtNamespace_AutodbLeavesNoPreparedStatementOnTheSession(t *testing.T) {
	cfg, err := pgx.ParseConfig(driverDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	// The simple protocol, so the CLIENT contributes no prepared statement
	// either and anything found belongs to autodb.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var n int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_prepared_statements").Scan(&n); err != nil {
		t.Fatalf("counting prepared statements: %v", err)
	}
	if n != 0 {
		var names string
		_ = conn.QueryRow(ctx,
			"SELECT string_agg(name, ', ') FROM pg_prepared_statements").Scan(&names)
		t.Errorf("the session carries %d prepared statement(s) the client never created: %s",
			n, names)
	}
}
