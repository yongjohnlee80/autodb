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

// readerWireSession is pgWireSession with the user and grant demoted to reader,
// the fixture's editor transaction rolled back, and a scratch table plus a
// smuggling function created by root. Returns the table and function names.
func readerWireSession(t *testing.T) (f *fixture, connID int64, sid SessionID, userID int64, table, fn string) {
	t.Helper()
	f, connID, sid, _, userID = pgWireSession(t)
	ctx := context.Background()
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture's transaction: %v", rb.err)
	}
	// Process-unique names: a failed cleanup must never collide with the next run.
	seq := time.Now().UnixNano()
	table = fmt.Sprintf("reader_raw_%d", seq)
	fn = fmt.Sprintf("reader_smuggle_%d", seq)
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS int LANGUAGE sql AS $$ INSERT INTO %s(note) VALUES ('smuggled'); SELECT 1 $$`, fn, table), testIP); err != nil {
		t.Skipf("cannot create the smuggling function: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP FUNCTION IF EXISTS "+fn+"()", testIP); err != nil {
			t.Logf("cleanup: drop function: %v", err)
		}
		if _, err := f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP); err != nil {
			t.Logf("cleanup: drop table: %v", err)
		}
	})
	// The wire PAT belongs to ROOT, so demoting its owner demotes root — and the
	// cleanup above runs with root's token. Restore the role FIRST (cleanups run
	// LIFO), or the drops are refused as a reader's DDL and the tables leak.
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

// THE CLAIM: a write smuggled through a volatile function is classified as a
// read, passes the gate, is dispatched — and PostgreSQL refuses it with 25006
// because the reader's unit runs in a READ ONLY transaction. The refusal is
// protocol data (ErrorResponse) with the target's own code; the session stays
// idle (the wrap is autodb's, not the client's); nothing was written.
func TestWireQueryReader_SmuggledWriteFailsAtTheTargetWith25006(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT %s()", fn))
	if r.err != nil {
		t.Fatalf("a target refusal must be protocol data, got Go error %v", r.err)
	}
	if len(r.dispatch) != 1 {
		t.Fatalf("the smuggled write must REACH the wire (classified as a read) — dispatches %d", len(r.dispatch))
	}
	er := kinds(r.msgs, "ErrorResponse")
	if len(er) != 1 || er[0].Err == nil || er[0].Err.Code != "25006" {
		t.Fatalf("ErrorResponse %+v, want the TARGET's 25006 read_only_sql_transaction — anything else means the reader's unit was not wrapped READ ONLY", er)
	}
	if r.status != TxStatusIdle {
		t.Fatalf("status %q, want I — the READ ONLY wrap is autodb's transaction, not the client's", r.status)
	}
	if got := rowCount(t, f, connID, table); got != "0" {
		t.Fatalf("table has %s rows: the smuggled INSERT was committed", got)
	}
}

// Inside a reader's OWN transaction the policy forces READ ONLY at BEGIN — even
// when the client asks for READ WRITE — and the smuggled write fails with 25006,
// aborting the transaction (E) until ROLLBACK.
func TestWireQueryReader_BeginReadWriteIsForcedReadOnlyAndSmuggledWriteAborts(t *testing.T) {
	f, connID, sid, userID, table, fn := readerWireSession(t)
	b := runRaw(t, f, sid, userID, "BEGIN READ WRITE")
	if b.err != nil || b.status != TxStatusInTx {
		t.Fatalf("BEGIN READ WRITE for a reader: status %q err %v — it is accepted and OVERRIDDEN, not refused", b.status, b.err)
	}
	if len(auditDetail(t, f, "tx_readonly_forced")) == 0 {
		t.Fatal("no tx_readonly_forced audit: the override must be on the record")
	}
	r := runRaw(t, f, sid, userID, fmt.Sprintf("SELECT %s()", fn))
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
	if got := rowCount(t, f, connID, table); got != "0" {
		t.Fatalf("table has %s rows", got)
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
