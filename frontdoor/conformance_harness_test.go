package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// THE CONFORMANCE HARNESS (matrix §10, criterion 7's frame): the listener on a
// real socket, driven by a real client library, asserted against matrix rows.
//
// Every other live cell in this package speaks the protocol by hand with
// pgproto3. That proves the front door answers the frames WE send it — which is
// necessary, and is not the same claim as "a driver works against it". A driver
// sends what it decides to send: its own startup parameters, its own statement
// shapes, its own idea of when to pipeline. This harness is where that claim
// lives.
//
// WHICH DRIVER, and the limitation stated rather than implied: pgx's pgconn is
// used because it is already a direct dependency and pgconn.Exec speaks the
// SIMPLE protocol. lib/pq — the client §10 actually names, and LM's real one —
// is NOT a dependency of this module, and adding one is not a decision a test
// harness makes on its own. Those arms are present and skipped with that
// reason, so the harness records which client shapes remain unproven instead of
// implying it covers them.
//
// lib/pq and pgx are not interchangeable witnesses: lib/pq uses the simple
// protocol for parameterless statements and pipelines readily, while pgx
// defaults to the extended protocol and speaks simple only through pgconn.Exec
// or an explicit query mode. Proving one does not prove the other.

// harnessConn opens a real client connection to a listener in front of a real
// engine, through the driver's own connection path.
func harnessConn(t *testing.T) (*pgconn.PgConn, func()) {
	t.Helper()
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})

	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	// sslmode=require: the front door has no plaintext path (row 2.1), so a
	// driver that would fall back must not be given the chance to hide it.
	dsn := fmt.Sprintf("postgres://root:%s@%s:%s/%s?sslmode=require",
		secret, host, port, database)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the harness DSN: %v", err)
	}
	cfg.TLSConfig.InsecureSkipVerify = true // the cell's own self-signed listener
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("a real driver could not connect to the front door: %v", err)
	}
	return conn, func() { _ = conn.Close(context.Background()) }
}

// Row 4:Query through a real driver's simple-protocol path.
func TestHarness_ADriverRunsAParameterlessQuery(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	res, err := conn.Exec(context.Background(), "SELECT 42").ReadAll()
	if err != nil {
		t.Fatalf("the driver's simple Query failed: %v", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || string(res[0].Rows[0][0]) != "42" {
		t.Fatalf("result = %#v, want a single row 42", res)
	}
}

// Row 4:Query's implicit block, in the shape psql sends it: several statements
// in ONE buffer. A later failure rolls back the earlier ones — the semantics the
// front door must not paper over.
func TestHarness_AMultiStatementBufferIsOneImplicitBlock(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()
	ctx := context.Background()

	table := fmt.Sprintf("fd_harness_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (n int)", table)).ReadAll(); err != nil {
		t.Fatalf("creating the scratch table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table)).ReadAll()
	})

	// One buffer, and the last statement fails.
	_, err := conn.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s VALUES (1); INSERT INTO %s VALUES (2); SELECT 1/0", table, table)).ReadAll()
	if err == nil {
		t.Fatal("the buffer's failing statement was not reported")
	}

	rows, rerr := conn.Exec(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).ReadAll()
	if rerr != nil {
		t.Fatalf("counting: %v", rerr)
	}
	if got := string(rows[0].Rows[0][0]); got != "0" {
		t.Fatalf("%s rows survived a failed implicit block — a multi-statement buffer outside an "+
			"explicit transaction is ONE block, and a later failure rolls back the earlier "+
			"statements (matrix row 4:Query)", got)
	}
}

// The #59 property through a REAL driver rather than a hand-built pair: two
// statements sent before either reply is read. Every cell that proved this
// before spoke pgproto3 directly; this proves a driver's own pipelining reaches
// the engine.
func TestHarness_ADriverPipelinesTwoStatements(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	// pgconn's multi-statement Exec writes one buffer and reads the replies
	// afterwards, which is the pipelined shape from the server's side.
	res, err := conn.Exec(context.Background(), "SELECT 1; SELECT 2").ReadAll()
	if err != nil {
		t.Fatalf("the driver's pipelined pair failed: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("%d results, want 2 — the second statement of a pipelined buffer was lost, which "+
			"is the defect PR #59 fixed, reaching here through a driver rather than a hand-built "+
			"frame pair", len(res))
	}
}

// A refusal must reach the driver AS a refusal — with a SQLSTATE it can act on,
// not as a broken connection. A front door that closed the socket instead would
// look to every driver like a network fault.
func TestHarness_ARefusalArrivesAsASQLSTATENotADisconnect(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()
	ctx := context.Background()

	_, err := conn.Exec(ctx, "SELECT 1/0").ReadAll()
	if err == nil {
		t.Fatal("division by zero was not reported")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the driver got %v, which is not a PgError — a target error must arrive as a "+
			"SQLSTATE the client can act on, never as a transport failure", err)
	}
	if pgErr.Code != "22012" {
		t.Fatalf("SQLSTATE = %s, want the target's own 22012", pgErr.Code)
	}

	// And the session SURVIVES it, which is what separates a statement error
	// from a session-ending refusal.
	if _, err := conn.Exec(ctx, "SELECT 1").ReadAll(); err != nil {
		t.Fatalf("the session did not survive a statement error: %v", err)
	}
}

// ---- Arms this harness names but cannot yet prove ----
//
// They are cells rather than a comment so that the harness is the one place a
// reader looks to learn which client shapes are covered, and so that unblocking
// one is a deletion of a skip rather than a hunt for where it should have gone.

func TestHarness_LibPQSimpleProtocol(t *testing.T) {
	t.Skip("matrix §10 names lib/pq + sqlx as LM's real client. lib/pq is NOT a dependency of this " +
		"module, and adding one is Johno's decision, not a test harness's. Unblocked by that approval; " +
		"pgx cannot stand in, because lib/pq uses the simple protocol for parameterless statements " +
		"while pgx speaks simple only through pgconn.Exec — proving one does not prove the other")
}

func TestHarness_LibPQParameterized(t *testing.T) {
	t.Skip("same dependency decision as TestHarness_LibPQSimpleProtocol, and additionally the " +
		"extended protocol: blocked on PR #57")
}

func TestHarness_PgxExtendedProtocol(t *testing.T) {
	t.Skip("pgx's default query mode is the extended protocol, which the front door refuses until " +
		"F2 lands (PR #57). This arm is the pgx-class suite §10 names — binary formats and the " +
		"statement cache — and it belongs here rather than in a second harness")
}
