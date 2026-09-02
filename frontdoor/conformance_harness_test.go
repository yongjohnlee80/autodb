package frontdoor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
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
// WHICH DRIVERS, and why more than one. §10 names lib/pq + sqlx as LM's real
// client and a pgx-class suite separately, because they are NOT interchangeable
// witnesses: lib/pq uses the SIMPLE protocol for parameterless statements and
// speaks database/sql's idioms, while pgx defaults to the EXTENDED protocol and
// speaks simple only through pgconn.Exec or an explicit query mode. A harness
// proving "a real driver works" against one of them does not prove the other,
// and the front door's behaviour differs by protocol.
//
// lib/pq and sqlx are TEST-ONLY dependencies, approved by Johno on that basis
// (2026-09-03). They are imported from this file and no other, and nothing in
// the production build reaches them — a guard below asserts that rather than
// leaving it to review.

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
	return conn, func() { _ = conn.Close(opCtx(t)) }
}

// Row 4:Query through a real driver's simple-protocol path.
func TestHarness_ADriverRunsAParameterlessQuery(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	res, err := conn.Exec(opCtx(t), "SELECT 42").ReadAll()
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

	table := fmt.Sprintf("fd_harness_%d", time.Now().UnixNano())
	if _, err := conn.Exec(opCtx(t), fmt.Sprintf("CREATE TABLE %s (n int)", table)).ReadAll(); err != nil {
		t.Fatalf("creating the scratch table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(opCtx(t), fmt.Sprintf("DROP TABLE IF EXISTS %s", table)).ReadAll()
	})

	// One buffer, and the last statement fails.
	_, err := conn.Exec(opCtx(t), fmt.Sprintf(
		"INSERT INTO %s VALUES (1); INSERT INTO %s VALUES (2); SELECT 1/0", table, table)).ReadAll()
	if err == nil {
		t.Fatal("the buffer's failing statement was not reported")
	}

	rows, rerr := conn.Exec(opCtx(t), fmt.Sprintf("SELECT count(*) FROM %s", table)).ReadAll()
	if rerr != nil {
		t.Fatalf("counting: %v", rerr)
	}
	if got := string(rows[0].Rows[0][0]); got != "0" {
		t.Fatalf("%s rows survived a failed implicit block — a multi-statement buffer outside an "+
			"explicit transaction is ONE block, and a later failure rolls back the earlier "+
			"statements (matrix row 4:Query)", got)
	}
}

// MF2 (lector r0): the previous version sent "SELECT 1; SELECT 2" through
// pgconn.Exec and called it pipelining. That is ONE Query frame carrying two
// statements — the implicit-block shape the cell above already covers — so it
// could not detect a stranded second frame, which is the entire defect PR #59
// fixed. The name claimed two frames; the wire carried one.
//
// This drives TWO Query frames into one flush on the driver's own connection,
// through the frontend pgconn exposes, and reads nothing until both are sent.
//
// BE PRECISE ABOUT WHAT THAT IS (r1 SF1): the frames are built by hand and
// pushed through a real driver's connection and TLS stack. It proves the front
// door handles the shape, on a real socket, under a real client's transport. It
// does NOT prove a high-level driver DECIDES to pipeline — no pgconn API asks
// for two Query frames before a read, which is why it is built this way. The
// driver-decides case belongs to lib/pq, and lib/pq cannot connect (see the
// datestyle finding below).
//
// IT CATCHES A STRANDED SECOND FRAME, and the control that proves it took two
// attempts — the first of which is worth recording.
//
// My first negative control mutated frameReader's SCAN, the validation, and the
// cell stayed green. A positive control (a panic in that arm) showed the path
// was executed, so the mutation ran and produced no symptom, which I could not
// explain — and I sent the PR with the claim WITHHELD rather than asserted.
//
// Lector named the reason: scan's state does not affect what Read RETURNS, so
// pgproto3 still received the whole buffer and nothing was ever stranded. I had
// mutated the part of the reader that decides, not the part that delivers. The
// control that works truncates what Read hands over after the first frame, and
// it reddens this cell with "only 0 of 2 readiness bytes".
//
// The lesson is narrower than "mutate carefully": a mutation must break the
// thing the cell OBSERVES, and I had broken something adjacent to it that the
// observation does not pass through.
func TestHarness_ADriverSendsTwoQueryFramesBeforeReading(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	fe := conn.Frontend()
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	fe.Send(&pgproto3.Query{String: "SELECT 2"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("flushing two Query frames: %v", err)
	}

	// BOUNDED ON THE SOCKET, not between reads (r1 MF1). A time.Now() check at
	// the top of the loop never runs while fe.Receive() is blocked in netFD.Read
	// — juliet reproduced the indefinite block when reply 2 is absent. The
	// deadline has to be on the connection, which is the same lesson as #66's
	// readiness drain, in a cell I wrote after learning it.
	if err := conn.Conn().SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("bounding the pipelined read: %v", err)
	}
	ready := 0
	for ready < 2 {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("only %d of 2 readiness bytes (%v): the second frame of a pipelined pair was "+
				"accepted by the driver, never seen by the engine, and nobody was told", ready, err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			ready++
		}
	}
}

// A refusal must reach the driver AS a refusal — with a SQLSTATE it can act on,
// not as a broken connection. A front door that closed the socket instead would
// look to every driver like a network fault.
func TestHarness_ARefusalArrivesAsASQLSTATENotADisconnect(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	_, err := conn.Exec(opCtx(t), "SELECT 1/0").ReadAll()
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
	if _, err := conn.Exec(opCtx(t), "SELECT 1").ReadAll(); err != nil {
		t.Fatalf("the session did not survive a statement error: %v", err)
	}
}

// ---- Arms this harness names but cannot yet prove ----
//
// They are cells rather than a comment so that the harness is the one place a
// reader looks to learn which client shapes are covered, and so that unblocking
// one is a deletion of a skip rather than a hunt for where it should have gone.

// LIB/PQ CANNOT CONNECT AT ALL, and that is a FINDING, not a broken cell.
//
// This is what the harness was for. Every hand-written pgproto3 cell in this
// package passes, and pgconn connects happily — because both send only the
// parameters the front door accepts. lib/pq sends one more, and is refused with
// the uniform denial before it ever runs a statement.
//
// MEASURED, not inferred. The audit line from the refusal reads:
//
//	fd.auth_denied  frontdoor/startup-parameter-refused  datestyle
//
// And it is UNCONDITIONAL: lib/pq's config normalization hard-codes it —
// connector.go:612, `cfg.ClientEncoding, cfg.Datestyle = "UTF8", "ISO, MDY"` —
// so every lib/pq connection sends `datestyle`, whatever the DSN says. There is
// no client-side option that avoids it.
//
// THE CONFLICT IS BETWEEN TWO PARTS OF THE MATRIX, not in the code. Row 3.1
// says "any other parameter → Refused (uniform denial)", and the front door does
// exactly that — correctly. §10 names "lib/pq + sqlx conformance (LM's real
// client)" as a conformance target. Both cannot hold: as specified, LM's real
// client can never open a session.
//
// Worth noting for whoever rules on it: the VALUE lib/pq sends is "ISO, MDY",
// PostgreSQL's own default, and it pairs with client_encoding=UTF8 which this
// surface already accepts. So the refusal is not protecting against a client
// asking for something unusual.
//
// NOT FIXED HERE. A matrix rule is not a thing a test harness changes, and the
// PR that adds cells changes no behaviour. Raised to jarvis for a ruling.
func TestHarness_LibPQIsRefusedForDatestyle(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, events, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	db, err := sql.Open("postgres", fmt.Sprintf(
		"host=%s port=%s user=root password=%s dbname=%s sslmode=require",
		host, port, secret, database))
	if err != nil {
		t.Fatalf("opening lib/pq: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(opCtx(t)); err == nil {
		t.Fatal("lib/pq CONNECTED — the datestyle refusal has been ruled on and lifted. Delete " +
			"this cell and restore the four driver arms below it; they are written and were " +
			"passing except for this")
	}

	// The refusal must be the one this cell names. If lib/pq is being refused
	// for some OTHER reason, the finding recorded here is wrong and the new
	// reason is a fresh one to chase.
	// WAIT FOR THE AUDIT, do not sample it. The client's Ping error returns as
	// soon as the denial frame arrives; the server writes its audit row after.
	// Reading events() immediately passed when this cell ran alone and failed
	// under full-suite load, reporting an EMPTY reason — which read exactly like
	// "the finding changed" and sent me chasing a behaviour change that had not
	// happened. A cell that samples a value written by another goroutine is
	// asking a question before the answer exists.
	waitFor(t, "the startup-parameter refusal to be audited", func() bool {
		for _, ev := range events() {
			if ev.Kind == "fd.auth_denied" {
				return true
			}
		}
		return false
	})
	refused := ""
	for _, ev := range events() {
		if ev.Kind == "fd.auth_denied" {
			refused = ev.Reason + "/" + ev.Detail
		}
	}
	if refused != "frontdoor/startup-parameter-refused/datestyle" {
		t.Fatalf("lib/pq was refused as %q, not for datestyle — the finding this cell pins has "+
			"changed and the new cause needs chasing", refused)
	}
}

// The arms that lib/pq would run, once it can connect. They are written rather
// than described: unblocking is then a deletion of the guard above, not a
// morning spent reconstructing what the cells were meant to assert.
func TestHarness_LibPQRunsAParameterlessQuery(t *testing.T) {
	t.Skip("blocked by TestHarness_LibPQIsRefusedForDatestyle — lib/pq cannot open a session " +
		"(matrix row 3.1 refuses `datestyle`, which lib/pq always sends). Awaiting a ruling")

	db, done := harnessDB(t)
	defer done()

	var n int
	if err := db.QueryRowContext(opCtx(t), "SELECT 42").Scan(&n); err != nil {
		t.Fatalf("lib/pq's simple query failed: %v", err)
	}
	if n != 42 {
		t.Fatalf("got %d, want 42", n)
	}

	// A target error must arrive as a *pq.Error with the target's own SQLSTATE,
	// and the pooled connection must remain usable. A front door that closed the
	// socket would look to database/sql like a bad connection and be silently
	// retried on another — turning one statement error into two executions.
	err := db.QueryRowContext(opCtx(t), "SELECT 1/0").Scan(&n)
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("lib/pq got %v, which is not a *pq.Error", err)
	}
	if string(pqErr.Code) != "22012" {
		t.Fatalf("SQLSTATE = %s, want 22012", pqErr.Code)
	}
	if err := db.QueryRowContext(opCtx(t), "SELECT 42").Scan(&n); err != nil || n != 42 {
		t.Fatalf("the connection did not survive a statement error: %v", err)
	}
}

func TestHarness_SqlxScansThroughTheFrontDoor(t *testing.T) {
	t.Skip("blocked by the same refusal: sqlx wraps a database/sql handle, and the handle is " +
		"lib/pq's")

	db, done := harnessDB(t)
	defer done()

	xdb := sqlx.NewDb(db, "postgres")
	var row struct {
		N int    `db:"n"`
		S string `db:"s"`
	}
	if err := xdb.GetContext(opCtx(t), &row, "SELECT 7 AS n, 'seven' AS s"); err != nil {
		t.Fatalf("sqlx.Get through the front door: %v", err)
	}
	if row.N != 7 || row.S != "seven" {
		t.Fatalf("got %+v, want {7 seven}", row)
	}
}

// harnessDB opens a database/sql handle over lib/pq against the front door.
func harnessDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})

	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("listener address %q is not host:port", addr)
	}
	// sslmode=require, not verify-full: the cell's listener is self-signed. The
	// front door has no plaintext path (row 2.1), so a driver that would fall
	// back must not be given the chance to hide it.
	db, err := sql.Open("postgres", fmt.Sprintf(
		"host=%s port=%s user=root password=%s dbname=%s sslmode=require",
		host, port, secret, database))
	if err != nil {
		t.Fatalf("opening lib/pq against the front door: %v", err)
	}
	// ONE connection: database/sql may otherwise open several and a cell
	// asserting that "the connection survived" would silently be handed a new
	// one, proving nothing.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(opCtx(t)); err != nil {
		t.Fatalf("lib/pq could not connect to the front door: %v", err)
	}
	return db, func() { _ = db.Close() }
}

// THE EXTENDED PROTOCOL through a real driver — F1 refused it, #57 serves it,
// and this arm's skip ("blocked on PR #57") went stale the moment that merged.
//
// It asserts the VALUE, not merely that the call returned. Everything on this
// path relays, so almost everything "runs": a cell that only checks for a nil
// error would pass against a front door that forwarded the segment to the wrong
// target, or returned another statement's rows (white-vision's F4 fixture spec
// makes this its central point, and it is the reason this arm is not a smoke
// test).
//
// The parameter is what makes it the extended path rather than a simple query
// wearing a cast: pgconn.ExecParams drives Parse/Bind/Describe/Execute/Sync
// with a typed argument, which is the segment shape lib/pq and JDBC send.
func TestHarness_PgxExtendedProtocol(t *testing.T) {
	conn, done := harnessConn(t)
	defer done()

	res := conn.ExecParams(opCtx(t), "SELECT $1::int + 1",
		[][]byte{[]byte("41")}, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("a real driver's extended-protocol query failed: %v", res.Err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("result shape = %v, want one row of one column", res.Rows)
	}
	if got := string(res.Rows[0][0]); got != "42" {
		t.Fatalf("got %q, want 42 — the parameter was bound and the expression evaluated at the "+
			"TARGET, so a wrong value here means the segment did not carry what the client sent",
			got)
	}
}

// Vision's remaining eight extended shapes (named-statement reuse, binary
// params/results, Describe-before-Execute, maxRows/PortalSuspended paging,
// discard-through-Sync, standalone and empty Flush, pipelined mixed
// simple+extended, control through the extended protocol) are a follow-up PR,
// not this one: four matrix rows are AWAITING for want of a client driving them
// (4:Describe, 4:Execute, 4:Flush, 4:discard), and closing them is a slice of
// its own rather than scope bolted onto a PR already under review.
//
// Spec: $KB_ROOT/agents/ultron-prime/incoming/2026-09-02-f4-extended-client-shapes-from-white-vision.md

// THE TEST-ONLY CONDITION, ASSERTED RATHER THAN REVIEWED.
//
// lib/pq and sqlx were approved as TEST-ONLY dependencies. A condition that
// lives only in a commit message is a condition nobody enforces: the next
// import from a production file would be caught, if at all, by a reviewer
// noticing. This fails the build instead, on the day it happens.
//
// It walks the repo's non-test Go files rather than trusting `go mod` layout,
// because go.mod does not distinguish a test-only requirement — the distinction
// is entirely about who IMPORTS them, which is what this reads.
func TestHarness_TheApprovedDriversStayTestOnly(t *testing.T) {
	root := repoRoot(t)
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == "github.com/lib/pq" || p == "github.com/jmoiron/sqlx" {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+" imports "+p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("lib/pq and sqlx are approved as TEST-ONLY dependencies, and these production "+
			"files import them: %v — the approval was for a conformance harness, and a driver "+
			"in the shipped binary is a different decision that has not been made", offenders)
	}
}

// opCtx bounds ONE live operation (MF3, lector r0).
//
// Every live call previously used context.Background(), so a lost frame or a
// withheld readiness byte hung until the outer go-test timeout — surfacing as
// "panic: test timed out" with no indication of which call stalled. A cell that
// hangs reports nothing; the rule that bounded #66's readiness drain applies to
// every operation that waits on the front door.
func opCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}
