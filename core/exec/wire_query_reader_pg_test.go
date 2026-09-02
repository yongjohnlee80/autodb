package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// ADR-0075 F3a, item 1, on the RAW producer: a reader's every unit runs inside a
// READ ONLY transaction at the target, so a write smuggled past the classifier —
// a volatile function whose body inserts — fails AT THE TARGET with SQLSTATE
// 25006. The classifier is the first gate; PostgreSQL is the one the product's
// claim rests on. Proven for the decoded WireExecute path in
// standing_authority_test; these cells prove it for WireQuery, which the F1
// loop drives.

// createReaderScratch creates the scratch table and the smuggling function as
// root. The TABLE's cleanup is registered the moment the table exists — before
// anything else can fail — so no exit path leaks it (PR #53 MF1); the function's
// cleanup is registered once the function exists. A failure to create the
// function is RETURNED, never skipped: an opted-in TEST_PGURL run that cannot
// build its fixture is a failing run, not a green one.
func createReaderScratch(t *testing.T, f *fixture, connID int64, fnBodyFor func(table string) string) (table, fn string, err error) {
	t.Helper()
	ctx := context.Background()
	seq := time.Now().UnixNano() // process-unique: a failed cleanup must never collide with the next run
	table = fmt.Sprintf("reader_raw_%d", seq)
	fn = fmt.Sprintf("reader_smuggle_%d", seq)
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		return "", "", fmt.Errorf("create table: %w", err)
	}
	t.Cleanup(func() {
		if _, derr := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP); derr != nil {
			t.Logf("cleanup: drop table %s: %v", table, derr)
		}
	})
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ %s $$`, fn, fnBodyFor(table)), testIP); err != nil {
		return table, "", fmt.Errorf("create smuggling function: %w", err)
	}
	t.Cleanup(func() {
		if _, derr := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP FUNCTION IF EXISTS "+fn+"()", testIP); derr != nil {
			t.Logf("cleanup: drop function %s: %v", fn, derr)
		}
	})
	return table, fn, nil
}

// readerWireSession is pgWireSession with the fixture's editor transaction
// rolled back, the scratch table and smuggling function created by root, and
// THEN the user and grant demoted to reader. Order matters twice over: the DDL
// runs while root is still admin, and the role-restoring cleanup is registered
// LAST so it runs FIRST (cleanups are LIFO) — the wire PAT belongs to root, so
// demoting its owner demotes root, and root's DDL cleanups would otherwise be
// refused as a reader's and leak the objects.
func readerWireSession(t *testing.T) (f *fixture, connID int64, sid SessionID, userID int64, table, fn string) {
	t.Helper()
	f, connID, sid, _, userID = pgWireSession(t)
	ctx := context.Background()
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture's transaction: %v", rb.err)
	}
	var err error
	table, fn, err = createReaderScratch(t, f, connID, func(tbl string) string {
		return "INSERT INTO " + tbl + "(note) VALUES ('smuggled'); SELECT 1"
	})
	if err != nil {
		t.Fatalf("reader fixture (opted-in TEST_PGURL run; a fixture that cannot be built is a failure, not a skip): %v", err)
	}
	// Restore root's role BEFORE the DDL cleanups run (registered after them ⇒ runs first).
	t.Cleanup(func() {
		_ = f.store.Users.OnCtx(context.Background()).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleAdmin).Update()
		_ = f.store.Grants.OnCtx(context.Background()).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleAdmin).Update()
	})
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	return f, connID, sid, userID, table, fn
}

// PR #53 MF1 isolation check: when the smuggling function CANNOT be created, the
// setup returns an error (the caller fails, never skips) and the already-created
// table does not leak — its cleanup was registered the moment it existed.
func TestWireQueryReader_FixtureFailureLeaksNothing(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK: %v", rb.err)
	}
	var table string
	t.Run("broken-fixture", func(t *testing.T) {
		var err error
		table, _, err = createReaderScratch(t, f, connID, func(string) string { return "THIS IS NOT SQL" })
		if err == nil {
			t.Fatal("a function with an invalid body was created")
		}
		if table == "" {
			t.Fatal("the table was not created before the function failed; the scenario under test did not occur")
		}
		out, qerr := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT count(*) FROM pg_tables WHERE tablename = '"+table+"'", testIP)
		if qerr != nil || fmt.Sprint(out.Rows[0][0]) != "1" {
			t.Fatalf("table %s should exist inside the sub-test: %v %v", table, out.Rows, qerr)
		}
	}) // the sub-test's cleanups have run here
	out, err := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT count(*) FROM pg_tables WHERE tablename = '"+table+"'", testIP)
	if err != nil || fmt.Sprint(out.Rows[0][0]) != "0" {
		t.Fatalf("table %s LEAKED after the fixture failed: count=%v err=%v", table, out.Rows, err)
	}
}

// setFixtureRole flips the wire PAT's owner (root) between roles; the fixture leaves
// it as reader and restores admin in a cleanup.
func setFixtureRole(t *testing.T, f *fixture, connID, userID int64, role string) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, role).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, role).Update(); err != nil {
		t.Fatal(err)
	}
}

// adminSequence creates a sequence as admin and returns to reader. A sequence's
// nextval is a CATALOG function that WRITES — the belt's witness.
func adminSequence(t *testing.T, f *fixture, connID, userID int64) string {
	t.Helper()
	seq := fmt.Sprintf("reader_seq_%d", time.Now().UnixNano())
	setFixtureRole(t, f, connID, userID, meta.RoleAdmin)
	if _, err := f.eng.Execute(context.Background(), f.rootTok, connID, "CREATE SEQUENCE "+seq, testIP); err != nil {
		t.Fatalf("create sequence: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP SEQUENCE IF EXISTS "+seq, testIP)
	})
	setFixtureRole(t, f, connID, userID, meta.RoleReader)
	return seq
}

func rowCount(t *testing.T, f *fixture, connID int64, table string) string {
	t.Helper()
	out, err := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT count(*) FROM "+table, testIP)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return fmt.Sprint(out.Rows[0][0])
}

// The classifier is the first gate: a reader's plain INSERT never reaches the
// wire. Positive control: the reader's SELECT works and reports I.
func TestWireQueryReader_PlainWriteRefusedBeforeDispatchAndReadWorks(t *testing.T) {
	f, connID, sid, userID, table, _ := readerWireSession(t)
	ins := runRaw(t, f, sid, userID, "INSERT INTO "+table+"(note) VALUES ('plain')")
	if !errors.Is(ins.err, auth.ErrDenied) || len(ins.dispatch) != 0 {
		t.Fatalf("reader INSERT: err %v dispatches %d, want ErrDenied and 0", ins.err, len(ins.dispatch))
	}
	sel := runRaw(t, f, sid, userID, "SELECT count(*) FROM "+table)
	if sel.err != nil || sel.status != TxStatusIdle || len(sel.dispatch) != 1 {
		t.Fatalf("reader SELECT: status %q err %v dispatches %d", sel.status, sel.err, len(sel.dispatch))
	}
	if got := rowCount(t, f, connID, table); got != "0" {
		t.Fatalf("table has %s rows after a refused INSERT, want 0", got)
	}
}

// Amendment 6 rule 2 — the analysis stage: a reader's call to a USER-DEFINED
// function is refused BEFORE dispatch (zero dispatches), in every spelling —
// bare, schema-qualified, quoted — with the front door's own refusal naming it.
// The READ ONLY wrap behind it is proven separately with a catalog function.
func TestWireQueryReader_UserFunctionCallRefusedBeforeDispatch(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	for _, sql := range []string{fmt.Sprintf("SELECT %s()", fn), fmt.Sprintf("SELECT public.%s()", fn), fmt.Sprintf("SELECT count(*) FROM %s WHERE %s() > 0", table, fn), fmt.Sprintf(`SELECT "%s"()`, fn)} {
		r := runRaw(t, f, sid, userID, sql)
		if !errors.Is(r.err, ErrReaderAdvancedPattern) {
			t.Fatalf("%q: err %v, want ErrReaderAdvancedPattern", sql, r.err)
		}
		if len(r.dispatch) != 0 || len(r.msgs) != 0 {
			t.Fatalf("%q reached the wire (%d dispatch, %d msgs); the stage must refuse before dispatch", sql, len(r.dispatch), len(r.msgs))
		}
	}
	if got := rowCount(t, f, connID, table); got != "0" {
		t.Fatalf("table has %s rows", got)
	}
}

// Catalog functions are the language: a reader's ordinary query full of them
// runs, including set_config — which is not an escape to plug (rule 1/3), because
// the READ ONLY wrap, not a name list, is what bounds a reader.
func TestWireQueryReader_CatalogFunctionsAllowed(t *testing.T) {
	f, _, sid, userID, table, _ := readerWireSession(t)
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT count(*), now() > '2000-01-01', pg_catalog.set_config('application_name', 'reader_probe', true), coalesce(max(id), 0) FROM %s", table))
	if r.err != nil || r.status != TxStatusIdle || len(r.dispatch) != 1 {
		t.Fatalf("catalog-function query: status %q err %v dispatches %d — the stage must not refuse the language", r.status, r.err, len(r.dispatch))
	}
	if n := len(kinds(r.msgs, "DataRow")); n != 1 {
		t.Fatalf("%d DataRows, want 1", n)
	}
}

// THE BELT behind the stage (F3a item 1): a catalog function that WRITES —
// nextval — passes the analysis (it is the language) and PostgreSQL refuses it
// with 25006 inside the reader's READ ONLY transaction. Protocol data, status I,
// nothing written.
func TestWireQueryReader_CatalogWriteStillFailsAtTheTargetWith25006(t *testing.T) {
	f, connID, sid, userID, _, _ := readerWireSession(t)
	seq := adminSequence(t, f, connID, userID)
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT nextval('%s')", seq))
	if r.err != nil {
		t.Fatalf("a target refusal must be protocol data, got %v", r.err)
	}
	if len(r.dispatch) != 1 {
		t.Fatalf("nextval must REACH the wire (catalog function) — dispatches %d", len(r.dispatch))
	}
	er := kinds(r.msgs, "ErrorResponse")
	if len(er) != 1 || er[0].Err == nil || er[0].Err.Code != "25006" {
		t.Fatalf("ErrorResponse %+v, want the TARGET's 25006 — the READ ONLY wrap is the belt behind the analysis", er)
	}
	if r.status != TxStatusIdle {
		t.Fatalf("status %q, want I", r.status)
	}
}

// Editors first (rule 1): the same user-defined function call runs for an editor and writes.
func TestWireQueryReader_EditorMayCallUserFunctions(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	setFixtureRole(t, f, connID, userID, meta.RoleEditor)
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT %s()", fn))
	if r.err != nil || len(r.dispatch) != 1 || len(kinds(r.msgs, "ErrorResponse")) != 0 {
		t.Fatalf("editor's user-function call: err %v dispatches %d errors %d — editors get PostgreSQL as it is", r.err, len(r.dispatch), len(kinds(r.msgs, "ErrorResponse")))
	}
	if got := rowCount(t, f, connID, table); got != "1" {
		t.Fatalf("table has %s rows after the editor's call, want 1", got)
	}
}

// The routine set is invalidated when DDL runs THROUGH autodb: a function created
// a moment ago is refused to readers immediately, not after the cache TTL.
func TestWireQueryReader_NewFunctionRefusedImmediatelyAfterDDLThroughAutodb(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	ctx := context.Background()
	// Warm the cache as a reader.
	if r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT %s()", fn)); !errors.Is(r.err, ErrReaderAdvancedPattern) {
		t.Fatalf("warm-up: %v", r.err)
	}
	// Root defines a second function through autodb. Root is demoted right now, so restore first.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleAdmin).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleAdmin).Update(); err != nil {
		t.Fatal(err)
	}
	fn2 := fn + "_late"
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('late'); SELECT 1 $$`, fn2, table), testIP); err != nil {
		t.Fatalf("create second function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP FUNCTION IF EXISTS "+fn2+"()", testIP)
	})
	// Back to reader.
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).Set(meta.UserRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Set(meta.GrantRole, meta.RoleReader).Update(); err != nil {
		t.Fatal(err)
	}
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT %s()", fn2))
	if !errors.Is(r.err, ErrReaderAdvancedPattern) || len(r.dispatch) != 0 {
		t.Fatalf("function created through autodb a moment ago: err %v dispatches %d — the routine set must be invalidated by DDL, not wait for the TTL", r.err, len(r.dispatch))
	}
}

// Procedural statements are refused for readers before dispatch: DO and CALL.
func TestWireQueryReader_ProceduralStatementsRefused(t *testing.T) {
	f, _, sid, userID, _, _ := readerWireSession(t)
	for _, sql := range []string{"DO $$ BEGIN PERFORM 1; END $$", "CALL some_procedure()"} {
		r := runRaw(t, f, sid, userID, sql)
		if r.err == nil || len(r.dispatch) != 0 {
			t.Fatalf("%q: err %v dispatches %d — must be refused before dispatch", sql, r.err, len(r.dispatch))
		}
	}
}

// Inside a reader's OWN transaction the policy forces READ ONLY at BEGIN — even
// when the client asks for READ WRITE — and a catalog write (nextval) fails with
// 25006, aborting the transaction (E) until ROLLBACK. (A user-defined function
// would be refused earlier by the analysis stage; the wrap is proven with the
// language itself.)
func TestWireQueryReader_BeginReadWriteIsForcedReadOnlyAndCatalogWriteAborts(t *testing.T) {
	f, connID, sid, userID, _, _ := readerWireSession(t)
	seq := adminSequence(t, f, connID, userID)
	b := runRaw(t, f, sid, userID, "BEGIN READ WRITE")
	if b.err != nil || b.status != TxStatusInTx {
		t.Fatalf("BEGIN READ WRITE for a reader: status %q err %v — it is accepted and OVERRIDDEN, not refused", b.status, b.err)
	}
	if len(auditDetail(t, f, "tx_readonly_forced")) == 0 {
		t.Fatal("no tx_readonly_forced audit: the override must be on the record")
	}
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT nextval('%s')", seq))
	if r.err != nil {
		t.Fatalf("target refusal must be protocol data, got %v", r.err)
	}
	if er := kinds(r.msgs, "ErrorResponse"); len(er) != 1 || er[0].Err.Code != "25006" {
		t.Fatalf("ErrorResponse %+v, want 25006 inside the forced READ ONLY transaction", er)
	}
	if r.status != TxStatusAborted {
		t.Fatalf("status %q, want E", r.status)
	}
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil || rb.status != TxStatusIdle {
		t.Fatalf("ROLLBACK: %q %v", rb.status, rb.err)
	}
}

// A reader cannot lift the wrap by hand: SET TRANSACTION READ WRITE is refused
// at the gate (non-LOCAL SET), never dispatched.
func TestWireQueryReader_CannotLiftReadOnlyBySetTransaction(t *testing.T) {
	f, _, sid, userID, _, _ := readerWireSession(t)
	for _, sql := range []string{"SET TRANSACTION READ WRITE", "SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE", "SET default_transaction_read_only = off"} {
		r := runRaw(t, f, sid, userID, sql)
		if r.err == nil || len(r.dispatch) != 0 {
			t.Fatalf("%q: err %v dispatches %d — must be refused before dispatch", sql, r.err, len(r.dispatch))
		}
		if !strings.Contains(r.err.Error(), "SET") {
			t.Fatalf("%q refused for an unexpected reason: %v", sql, r.err)
		}
	}
}
