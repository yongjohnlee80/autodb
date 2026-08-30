package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Live-postgres session e2e, gated on TEST_PGURL.
//
// Everything above this file is tested against fakes or sqlite, and sqlite
// cannot host a session at all — it has no context finalizers, so
// SessionTxBeginner refuses it. That means the pinned-transaction path has
// never actually run until here: whether a statement really lands inside an
// uncommitted transaction, whether the belt is really armed, whether SET
// LOCAL really reverts, are all claims a fake would happily agree with.

// pgSession builds a session on a live postgres target with a scratch table.
func pgSession(t *testing.T) (*fixture, int64, SessionID, string) {
	t.Helper()

	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live session tests")
	}
	ctx := context.Background()
	f := newFixture(t)

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-session", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	// The session profile lives on the connection row, so the test enables
	// it the way a deployment would.
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}

	table := fmt.Sprintf("sess_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		// Bounded, and best-effort. A DROP needs an ACCESS EXCLUSIVE lock, so
		// a test that fails with a transaction still open would block here
		// forever and hang the whole package — turning one failing assertion
		// into a suite nobody can run. A leaked table in a scratch database
		// is the cheaper loss.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Logf("dropping %s timed out — a transaction is still holding locks on it", table)
		}
	})

	sid, err := f.eng.OpenSession(ctx, f.rootTok, connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(context.Background(), f.rootTok, sid, testIP) })
	return f, connID, sid, table
}

// countRows reads through the POOL, outside any session transaction, which
// is what makes it able to see whether work is still uncommitted.
func countRows(t *testing.T, f *fixture, connID int64, table string) int {
	t.Helper()

	res, err := f.eng.Execute(context.Background(), f.rootTok, connID, "SELECT COUNT(*) FROM "+table, testIP)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("count returned %d rows", len(res.Rows))
	}
	n, ok := res.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("count is %T, not an integer", res.Rows[0][0])
	}
	return int(n)
}

// The whole point: a statement inside a session transaction is NOT visible
// outside it until COMMIT. A fake cannot fail this test; a broken pin can.
func TestSessionPG_WorkIsInvisibleUntilCommit(t *testing.T) {
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"INSERT INTO "+table+" (note) VALUES ('pinned')", testIP); err != nil {
		t.Fatalf("insert inside the transaction: %v", err)
	}
	if n := countRows(t, f, connID, table); n != 0 {
		t.Fatalf("the pool sees %d rows before COMMIT — the statement did not run inside the "+
			"session's transaction, so nothing about the pin is working", n)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if n := countRows(t, f, connID, table); n != 1 {
		t.Fatalf("the pool sees %d rows after COMMIT, want 1", n)
	}
}

// And ROLLBACK discards it.
func TestSessionPG_RollbackDiscards(t *testing.T) {
	f, connID, sid, table := pgSession(t)

	mustSession(t, f, sid, "BEGIN")
	mustSession(t, f, sid, "INSERT INTO "+table+" (note) VALUES ('doomed')")
	mustSession(t, f, sid, "ROLLBACK")
	if n := countRows(t, f, connID, table); n != 0 {
		t.Errorf("the pool sees %d rows after ROLLBACK, want 0", n)
	}
}

// A session closed with work in flight rolls it back: a client that
// disappears must not leave a transaction holding locks.
//
// Counting rows is NOT enough to prove that, and finding out why was worth
// the detour. Under MVCC an uncommitted INSERT is invisible to a later
// SELECT anyway, so "the pool sees 0 rows" is equally true of a transaction
// that was rolled back and one that is still sitting open — the assertion
// passed with the rollback deleted, and then the cleanup DROP TABLE blocked
// forever on the lock nobody released.
//
// So the real property is asserted instead: after the close, DDL on that
// table proceeds. That is exactly the production hazard — an abandoned
// transaction blocking everyone else — and it fails in bounded time rather
// than hanging the suite.
func TestSessionPG_CloseRollsBackOpenWork(t *testing.T) {
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	mustSession(t, f, sid, "BEGIN")
	mustSession(t, f, sid, "INSERT INTO "+table+" (note) VALUES ('abandoned')")

	if err := f.eng.CloseSession(ctx, f.rootTok, sid, testIP); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if n := countRows(t, f, connID, table); n != 0 {
		t.Errorf("closing the session left %d rows committed, want 0", n)
	}

	// The lock is really gone: this needs an ACCESS EXCLUSIVE lock, which an
	// open transaction holding a row lock on the table would block.
	done := make(chan error, 1)
	go func() {
		_, err := f.eng.Execute(context.Background(), f.rootTok, connID,
			"ALTER TABLE "+table+" ADD COLUMN closed_ok INT", testIP)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DDL after the close: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("DDL blocked after the session closed — the abandoned transaction is still open " +
			"and holding locks, which is the production incident this rollback exists to prevent")
	}

	// And the session is gone.
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT 1", testIP); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a closed session answered %v, want ErrSessionNotFound", err)
	}
}

// One transaction per session.
func TestSessionPG_OneTransactionPerSession(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()

	mustSession(t, f, sid, "BEGIN")
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); !errors.Is(err, ErrTxAlreadyOpen) {
		t.Errorf("second BEGIN = %v, want ErrTxAlreadyOpen", err)
	}
	mustSession(t, f, sid, "ROLLBACK")
}

// The server-side belt is really armed, and BEHIND the engine's deadline.
// Read from inside the transaction, which is the only place a LOCAL setting
// is observable.
func TestSessionPG_ServerBeltIsArmedBehindTheEngineDeadline(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()

	mustSession(t, f, sid, "BEGIN")
	res, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"SELECT setting FROM pg_settings WHERE name = 'idle_in_transaction_session_timeout'", testIP)
	if err != nil {
		t.Fatalf("reading the belt: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("pg_settings returned %d rows", len(res.Rows))
	}
	got := fmt.Sprint(res.Rows[0][0]) // milliseconds, as a string
	want := fmt.Sprint(int(defaultTxLimits().serverBeltSeconds()) * 1000)
	if got != want {
		t.Errorf("idle_in_transaction_session_timeout = %sms, want %sms — the engine's belt "+
			"was not armed on the pinned transaction", got, want)
	}
	mustSession(t, f, sid, "ROLLBACK")
}

// SET LOCAL applies inside the transaction and is GONE after it, which is
// the property that makes admitting it safe on a pooled connection.
func TestSessionPG_SetLocalRevertsAtTheBoundary(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()

	readLockTimeout := func(where string) string {
		t.Helper()
		res, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
			"SELECT setting FROM pg_settings WHERE name = 'lock_timeout'", testIP)
		if err != nil {
			t.Fatalf("reading lock_timeout %s: %v", where, err)
		}
		return fmt.Sprint(res.Rows[0][0])
	}

	mustSession(t, f, sid, "BEGIN")
	mustSession(t, f, sid, "SET LOCAL lock_timeout = '5s'")
	if got := readLockTimeout("inside the transaction"); got != "5000" {
		t.Fatalf("lock_timeout inside the transaction = %sms, want 5000ms — SET LOCAL did not take effect", got)
	}
	mustSession(t, f, sid, "COMMIT")

	// After the boundary the setting is gone. If it were not, it would be
	// sitting on a pooled connection waiting for the next user.
	mustSession(t, f, sid, "BEGIN")
	if got := readLockTimeout("in a later transaction"); got == "5000" {
		t.Error("lock_timeout survived the transaction boundary — SET LOCAL leaked onto the " +
			"pooled connection, which is the exact failure admitting it is supposed to be safe from")
	}
	mustSession(t, f, sid, "ROLLBACK")

	// A non-LOCAL SET is refused before it can do that on purpose.
	_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SET lock_timeout = '5s'", testIP)
	if !errors.Is(err, ErrSetNotLocal) {
		t.Errorf("non-LOCAL SET = %v, want ErrSetNotLocal", err)
	}
}

// A failed statement aborts the transaction, the engine says so once, and
// ROLLBACK is the way out.
func TestSessionPG_AbortedTransactionAcceptsOnlyRollback(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	mustSession(t, f, sid, "BEGIN")
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT 1/0", testIP); err == nil {
		t.Fatal("the poisoning statement unexpectedly succeeded")
	}

	// Every further statement gets ONE clear engine answer rather than an
	// identical 25P02 relayed per statement.
	_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT 1", testIP)
	if !errors.Is(err, ErrTxAborted) {
		t.Errorf("statement in an aborted transaction = %v, want ErrTxAborted", err)
	}
	// Including a COMMIT, which PostgreSQL would answer with a rollback
	// anyway — reporting that as success would be a lie about the work.
	_, err = f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP)
	if !errors.Is(err, ErrTxAborted) {
		t.Errorf("COMMIT on an aborted transaction = %v, want ErrTxAborted", err)
	}
	// ROLLBACK is the recovery, and the session works afterwards.
	mustSession(t, f, sid, "ROLLBACK")
	mustSession(t, f, sid, "SELECT 1")
	_ = table
}

// The idle-in-transaction limit really rolls a transaction back, and the
// audit says which limit fired. Driven by the injected clock, so the test
// does not wait 90 seconds to prove a 90-second rule.
func TestSessionPG_IdleInTransactionRollsBackAndAudits(t *testing.T) {
	f, connID, sid, table := pgSession(t)
	ctx := context.Background()

	mustSession(t, f, sid, "BEGIN")
	mustSession(t, f, sid, "INSERT INTO "+table+" (note) VALUES ('timed-out')")

	// Well past the idle bound.
	if n := f.eng.reapExpired(ctx, time.Now().Add(10*time.Minute)); n != 1 {
		t.Fatalf("the reaper acted on %d sessions, want 1", n)
	}
	if n := countRows(t, f, connID, table); n != 0 {
		t.Errorf("the timed-out transaction left %d rows committed, want 0", n)
	}

	// The audit names WHICH limit fired — a timeout rollback must be
	// distinguishable from every other kind.
	rows, err := f.store.Audit.OnCtx(ctx).With(meta.AuditAction, "tx_rolled_back").Select()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if strings.Contains(r.Detail, "idle-in-transaction") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit record names the idle-in-transaction limit; records = %d", len(rows))
	}

	// The session survives its transaction being reaped and can open another.
	mustSession(t, f, sid, "BEGIN")
	mustSession(t, f, sid, "ROLLBACK")
}

func mustSession(t *testing.T, f *fixture, sid SessionID, sql string) {
	t.Helper()

	if _, err := f.eng.SessionExecute(context.Background(), f.rootTok, sid, sql, testIP); err != nil {
		t.Fatalf("SessionExecute(%q): %v", sql, err)
	}
}
