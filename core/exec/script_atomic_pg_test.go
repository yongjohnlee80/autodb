package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The R5 gate, against a live PostgreSQL: a query editor running
// `BEGIN; …; COMMIT;` gets ONE transaction.
//
// This cannot be tested against SQLite — it has no context finalizers, so it
// cannot host a session at all — and it cannot be faked, because the whole
// question is whether the boundary reached a real server on a pinned
// connection rather than being scattered across a pool.

// pgAtomicTarget builds a session-profile postgres connection and a scratch
// table, WITHOUT opening a session: the atomic path opens its own.
func pgAtomicTarget(t *testing.T) (*fixture, int64, string) {
	t.Helper()

	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live atomic-script tests")
	}
	ctx := context.Background()
	f := newFixture(t)

	connID, err := f.eng.CreateConnection(ctx, f.rootTok, "pg-atomic", "postgres", dsn, testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}

	table := fmt.Sprintf("atomic_%d", time.Now().UnixNano())
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"CREATE TABLE "+table+" (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)", testIP); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = f.eng.Execute(context.Background(), f.rootTok, connID, "DROP TABLE IF EXISTS "+table, testIP)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	return f, connID, table
}

func atomicRowCount(t *testing.T, f *fixture, connID int64, table string) int64 {
	t.Helper()
	res, err := f.eng.Execute(context.Background(), f.rootTok, connID,
		"SELECT count(*) FROM "+table, testIP)
	if err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("unexpected count shape: %#v", res.Rows)
	}
	switch v := res.Rows[0][0].(type) {
	case int64:
		return v
	default:
		t.Fatalf("count came back as %T", v)
		return 0
	}
}

func TestExecuteScriptAtomic_TheEditorGate(t *testing.T) {
	t.Run("a committed script applies everything", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		out, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"BEGIN; INSERT INTO "+table+" (note) VALUES ('a'); "+
				"INSERT INTO "+table+" (note) VALUES ('b'); COMMIT;", testIP)
		if err != nil {
			t.Fatalf("ExecuteScriptAtomic: %v", err)
		}
		if out.Statements != 4 {
			t.Errorf("ran %d statements, want 4", out.Statements)
		}
		if n := atomicRowCount(t, f, connID, table); n != 2 {
			t.Fatalf("%d rows after a committed script, want 2 — the COMMIT did not apply the "+
				"work the BEGIN opened", n)
		}
	})

	// THE GATE. A statement fails in the middle; nothing may be applied.
	// Down the statement-by-statement path the first INSERT would already be
	// committed and visible, which is what a person typing BEGIN believes
	// they have prevented.
	t.Run("a failure rolls back everything before it", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		_, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"BEGIN; INSERT INTO "+table+" (note) VALUES ('a'); "+
				"INSERT INTO "+table+" (note) VALUES (NULL); COMMIT;", testIP)
		if err == nil {
			t.Fatal("a script whose third statement violates NOT NULL returned success")
		}
		if !strings.Contains(err.Error(), "rolled back") {
			t.Errorf("the error does not say the work was rolled back, so a caller cannot tell "+
				"this from the partial-application path: %v", err)
		}
		if n := atomicRowCount(t, f, connID, table); n != 0 {
			t.Fatalf("%d rows survived a failed transactional script, want 0 — the script was "+
				"NOT atomic and the statements before the failure are applied", n)
		}
	})

	// A script that opens a transaction and never closes it. The session's
	// close is what rolls it back; nothing in the script says to.
	t.Run("a script that never commits applies nothing", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		out, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"BEGIN; INSERT INTO "+table+" (note) VALUES ('a');", testIP)
		if err != nil {
			t.Fatalf("ExecuteScriptAtomic: %v", err)
		}
		if out.Statements != 2 {
			t.Errorf("ran %d statements, want 2", out.Statements)
		}
		if n := atomicRowCount(t, f, connID, table); n != 0 {
			t.Fatalf("%d rows from a script that never committed, want 0 — an unclosed "+
				"transaction was left to apply itself", n)
		}
	})

	// The default is UNCHANGED. A script with no boundary is still a
	// sequence of independent statements, partial application included —
	// that behaviour is correct and people rely on it.
	t.Run("a script with no transaction is still statement-by-statement", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		_, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"INSERT INTO "+table+" (note) VALUES ('a'); INSERT INTO "+table+" (note) VALUES (NULL);", testIP)
		if err == nil {
			t.Fatal("the failing statement returned success")
		}
		if !strings.Contains(err.Error(), "already ran") {
			t.Errorf("a non-transactional script must still say the earlier statements ran: %v", err)
		}
		if n := atomicRowCount(t, f, connID, table); n != 1 {
			t.Fatalf("%d rows, want 1 — a script with no BEGIN must keep its previous "+
				"statement-by-statement behaviour, not silently become a transaction", n)
		}
	})

	// The session left behind is closed. An atomic script that leaked its
	// ephemeral session would consume a session slot per editor run and hit
	// the per-user cap after eight.
	t.Run("the ephemeral session does not leak", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		for i := 0; i < 12; i++ {
			if _, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
				"BEGIN; INSERT INTO "+table+" (note) VALUES ('x'); COMMIT;", testIP); err != nil {
				t.Fatalf("run %d of 12 failed (%v) — the per-user session cap is 8, so a leak "+
					"shows up here", i+1, err)
			}
		}
		if n := len(f.eng.sessions.snapshot()); n != 0 {
			t.Errorf("%d sessions are still open after 12 atomic scripts", n)
		}
	})
}

// A transaction verb must never reach a POOLED connection.
//
// This is a live regression cell for a defect R5 found in R3's own code:
// Profile.admit assumed its caller was SessionExecute, so it returned nil for
// BEGIN on a session-profile connection — including on the STATELESS path,
// where there is no session to transition and the statement is forwarded as
// text. Against a live PostgreSQL, `Execute(…, "BEGIN")` returned SUCCESS:
// a transaction was opened on a pooled physical connection and returned to
// the pool, leaving its state for whoever got that connection next. That is
// the exact hazard the v1compat refusal message describes, reintroduced by
// the profile meant to be safer.
//
// A fake cannot show this — the question is whether a real server ended up
// inside a transaction on a connection the pool then handed on.
func TestStatelessPath_RefusesTransactionControlOnASessionProfile(t *testing.T) {
	f, connID, table := pgAtomicTarget(t)
	ctx := context.Background()

	// Positive control: ordinary statements still run on this connection, so
	// a refusal below is about the VERB and not about the connection being
	// unusable.
	if _, err := f.eng.Execute(ctx, f.rootTok, connID, "SELECT 1", testIP); err != nil {
		t.Fatalf("an ordinary statement failed (%v); this test cannot observe the refusal either", err)
	}

	for _, sql := range []string{"BEGIN", "START TRANSACTION", "COMMIT", "ROLLBACK"} {
		res, err := f.eng.Execute(ctx, f.rootTok, connID, sql, testIP)
		if err == nil {
			t.Fatalf("stateless Execute ran %q (result=%v): a transaction verb reached a pooled "+
				"connection, and its state is now waiting for the next user of that connection",
				sql, res != nil)
		}
		if !errors.Is(err, ErrStatementUnsupported) {
			t.Errorf("%q was refused for the wrong reason: %v", sql, err)
		}
		if !strings.Contains(err.Error(), "exec.session_open") {
			t.Errorf("the refusal for %q does not tell the caller what to do instead: %v", sql, err)
		}
	}

	// And the connection is not left in a broken state by the refusals.
	if _, err := f.eng.Execute(ctx, f.rootTok, connID,
		"INSERT INTO "+table+" (note) VALUES ('after')", testIP); err != nil {
		t.Errorf("the connection was unusable after the refusals: %v", err)
	}
}

// MF1. The session path is chosen when a boundary appears ANYWHERE, so a
// statement before BEGIN — or after COMMIT — runs on the session with no
// transaction around it and is applied. The failure message said "nothing in
// this script was applied", which is a promise the code does not keep.
//
// The fix is not to refuse these scripts: `SELECT 1; BEGIN; …; COMMIT;` is a
// reasonable thing to type, and outside-the-boundary statements ARE applied
// under ordinary SQL semantics. The fix is to say so exactly.
func TestExecuteScriptAtomic_ReportsWorkOutsideTheTransactionHonestly(t *testing.T) {
	t.Run("a statement BEFORE the boundary is applied and the message says so", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		_, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"INSERT INTO "+table+" (note) VALUES ('outside'); "+
				"BEGIN; INSERT INTO "+table+" (note) VALUES ('inside'); "+
				"INSERT INTO "+table+" (note) VALUES (NULL); COMMIT;", testIP)
		if err == nil {
			t.Fatal("the NOT NULL violation returned success")
		}
		n := atomicRowCount(t, f, connID, table)
		if n != 1 {
			t.Fatalf("%d rows, want 1: the statement before BEGIN runs outside the "+
				"transaction and is applied", n)
		}
		if strings.Contains(err.Error(), "nothing in this script was applied") {
			t.Errorf("the message claims nothing was applied, but a row survived — a false "+
				"promise of atomicity is worse than no promise: %v", err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "outside") {
			t.Errorf("the message does not tell the caller that work ran outside the "+
				"transaction and is still applied: %v", err)
		}
	})

	t.Run("a statement AFTER the boundary is applied and the message says so", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		_, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"BEGIN; INSERT INTO "+table+" (note) VALUES ('committed'); COMMIT; "+
				"INSERT INTO "+table+" (note) VALUES (NULL);", testIP)
		if err == nil {
			t.Fatal("the NOT NULL violation returned success")
		}
		n := atomicRowCount(t, f, connID, table)
		if n != 1 {
			t.Fatalf("%d rows, want 1: the committed transaction is applied and the failure "+
				"came after it", n)
		}
		if strings.Contains(err.Error(), "rolled back") {
			t.Errorf("the message claims a rollback, but the transaction had already "+
				"COMMITTED before the failing statement: %v", err)
		}
	})

	// And the whole-script case still gets the strong promise, because there
	// the promise is true.
	t.Run("a well-formed envelope keeps the atomicity promise", func(t *testing.T) {
		f, connID, table := pgAtomicTarget(t)
		_, err := f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
			"BEGIN; INSERT INTO "+table+" (note) VALUES ('a'); "+
				"INSERT INTO "+table+" (note) VALUES (NULL); COMMIT;", testIP)
		if err == nil {
			t.Fatal("the NOT NULL violation returned success")
		}
		if n := atomicRowCount(t, f, connID, table); n != 0 {
			t.Fatalf("%d rows survived, want 0", n)
		}
		if !strings.Contains(err.Error(), "nothing in this script was applied") {
			t.Errorf("a script that IS one transaction should still get the strong "+
				"promise: %v", err)
		}
	})
}

// MF2. The ephemeral session's cleanup must not depend on the caller's token
// still authenticating.
//
// It used to: the deferred close went through the PUBLIC CloseSession, which
// re-validates the token. A logout or an admin revocation while a statement
// was running made cleanup fail authentication, and the session — with its
// pinned transaction and the connection under it — stayed registered with
// nobody able to close it. An engine does not ask permission to clean up
// after itself.
func TestExecuteScriptAtomic_CleansUpWhenTheTokenDiesMidScript(t *testing.T) {
	f, connID, table := pgAtomicTarget(t)
	ctx := context.Background()

	// Revoke the caller's auth session while the script is in flight. The
	// script's own statements are already authorized; what breaks is the
	// CLEANUP's re-authentication.
	go func() {
		time.Sleep(150 * time.Millisecond)
		uid := userIDOf(t, f)
		_ = f.store.Sessions.OnCtx(context.Background()).With(meta.SessUserID, uid).
			Set(meta.SessRevoked, int64(1)).Update()
	}()

	_, _ = f.eng.ExecuteScriptAtomic(ctx, f.rootTok, connID,
		"BEGIN; SELECT pg_sleep(0.5); INSERT INTO "+table+" (note) VALUES ('x'); COMMIT;", testIP)

	// THE EFFECT: whatever the script's own outcome, no session may be left
	// behind. One that survives holds a pinned transaction on the target and
	// there is no longer a valid token that could close it.
	if n := len(f.eng.sessions.snapshot()); n != 0 {
		t.Fatalf("%d ephemeral session(s) survived the script after the token was revoked — "+
			"each holds a pinned transaction on the target that nothing can now close", n)
	}
}

// The cap under CONCURRENCY, which lector asked to be pinned: the submitted
// leak cell was sequential, and sequential is the easy case. Eight concurrent
// transactional scripts fill the per-user cap; the ninth must be refused
// cleanly rather than hang or leak, every admitted one must finish, and the
// registry must drain.
func TestExecuteScriptAtomic_ConcurrentScriptsRespectTheCapAndDrain(t *testing.T) {
	f, connID, table := pgAtomicTarget(t)

	const n = 9 // the per-user cap is 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.eng.ExecuteScriptAtomic(context.Background(), f.rootTok, connID,
				"BEGIN; SELECT pg_sleep(0.4); INSERT INTO "+table+" (note) VALUES ('c'); COMMIT;", testIP)
		}(i)
	}
	wg.Wait()

	var admitted, refused int
	for i, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrSessionCapExceeded):
			refused++
		default:
			t.Errorf("script %d failed for an unexpected reason: %v", i, err)
		}
	}
	if admitted == 0 {
		t.Fatal("no concurrent script completed; the cap is refusing work it should admit")
	}
	if admitted+refused != n {
		t.Fatalf("admitted %d + refused %d != %d", admitted, refused, n)
	}
	// Every admitted script committed its row.
	if got := atomicRowCount(t, f, connID, table); got != int64(admitted) {
		t.Errorf("%d rows for %d admitted scripts — an admitted script did not commit", got, admitted)
	}
	// And nothing is left holding a connection.
	if left := len(f.eng.sessions.snapshot()); left != 0 {
		t.Errorf("%d sessions survived %d concurrent scripts", left, n)
	}
}
