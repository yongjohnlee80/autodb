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
