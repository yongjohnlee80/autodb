package exec

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// attempt_test.go holds what schema script 000003 gives every attempted
// statement: an identity minted at the dispatch decision, the epoch of the
// daemon that made it, and both carried on every row about it.

var attemptIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestAnAttemptCarriesItsIdentityOwnerAndConnection(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	rows, err := f.store.History.OnCtx(ctx).Select()
	if err != nil || len(rows) != 1 {
		t.Fatalf("history rows %d (%v), want 1", len(rows), err)
	}
	h := rows[0]
	if !attemptIDShape.MatchString(h.AttemptID) {
		t.Fatalf("history attempt_id %q is not 32 hex characters", h.AttemptID)
	}
	if h.AttemptOwner == "" || h.AttemptOwner != f.eng.OwnerEpoch() {
		t.Errorf("history attempt_owner %q, want this engine's epoch %q", h.AttemptOwner, f.eng.OwnerEpoch())
	}
	// Both audit rows about the attempt name it, and the connection: the trail
	// is read by a query on the columns, never by parsing detail.
	for _, action := range []string{"exec", "exec_result"} {
		a := f.audits(t, action)
		if len(a) != 1 {
			t.Fatalf("%d %q audit rows, want 1", len(a), action)
		}
		if a[0].AttemptID != h.AttemptID || a[0].ConnID != f.connID {
			t.Errorf("%q audit row: attempt %q conn %d, want attempt %q conn %d",
				action, a[0].AttemptID, a[0].ConnID, h.AttemptID, f.connID)
		}
	}
}

// Two identical asks are two attempts: nothing derives the identity from the
// SQL, so running the same text again records a new one.
func TestTheSameStatementTwiceIsTwoAttempts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, f.rootTok, "SELECT 1")
	f.exec(t, f.rootTok, "SELECT 1")
	rows, err := f.store.History.OnCtx(ctx).Select()
	if err != nil || len(rows) != 2 {
		t.Fatalf("history rows %d (%v), want 2", len(rows), err)
	}
	if rows[0].AttemptID == rows[1].AttemptID {
		t.Errorf("both runs carry attempt %q; two asks must be two attempts", rows[0].AttemptID)
	}
}

// A refusal after the attempt is recorded names the attempt and connection
// too: it is that attempt's terminal.
func TestARefusedAttemptsAuditNamesTheAttempt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	att, err := f.eng.recordAttempt(ctx, rootIdent(t, f), f.connID, testIP, "SELECT 1", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.eng.rejectRecordedAttempt(ctx, rootIdent(t, f), f.connID, testIP, "SELECT 1", att, errRefusedForTest)
	a := f.audits(t, "exec_rejected")
	if len(a) != 1 || a[0].AttemptID != att.ID || a[0].ConnID != f.connID {
		t.Fatalf("exec_rejected rows %+v, want one naming attempt %q and conn %d", a, att.ID, f.connID)
	}
}

// The owner is never empty: an empty owner is what a row from before 000003
// carries, and recovery settles those — an engine that stamped ” would have
// its own live attempts settled under it.
func TestAnEngineAlwaysHasAnOwnerEpoch(t *testing.T) {
	f := newFixture(t)
	if !attemptIDShape.MatchString(f.eng.OwnerEpoch()) {
		t.Fatalf("default owner epoch %q, want a minted 32-hex epoch", f.eng.OwnerEpoch())
	}
	const given = "0123456789abcdef0123456789abcdef"
	if got := New(f.store, f.svc, WithOwnerEpoch(given)).OwnerEpoch(); got != given {
		t.Errorf("WithOwnerEpoch(%q) gave %q", given, got)
	}
}

var errRefusedForTest = errors.New("refused by the test")

func rootIdent(t *testing.T, f *fixture) auth.Identity {
	t.Helper()
	id, err := f.svc.ValidateToken(context.Background(), f.rootTok)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
