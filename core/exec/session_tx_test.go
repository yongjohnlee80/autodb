package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/golib/dao"
)

// ADR-0074 §3 — transaction verbs are state transitions. These cover the
// transitions themselves; the live end-to-end path against a real PostgreSQL
// is exercised in the pg-tagged suite.

func TestTxID_IsUniqueAndPrefixed(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := newTxID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate transaction id %q — the audit trail would merge two transactions", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "tx_") {
			t.Fatalf("id %q lacks the tx_ prefix that makes it recognizable in an audit record", id)
		}
	}
}

func TestTxPhase_String(t *testing.T) {
	t.Parallel()

	for phase, want := range map[txPhase]string{txNone: "none", txActive: "active", txAborted: "aborted"} {
		if got := phase.String(); got != want {
			t.Errorf("txPhase(%d) = %q, want %q", int(phase), got, want)
		}
	}
}

// A statement error that leaves the transaction unusable moves the session
// into the aborted phase, so the engine can answer once instead of relaying
// an identical server error per statement.
func TestSession_AbortedPhaseFollowsTheServer(t *testing.T) {
	t.Parallel()

	s := &session{id: "x", txPhase: txActive}

	// An ordinary failure does not abort the transaction: a unique-violation
	// inside a transaction leaves it perfectly usable, and treating it as
	// terminal would throw away work the caller can still commit.
	s.noteStatementOutcome(errors.New("some ordinary failure"))
	if s.txPhase != txActive {
		t.Fatalf("phase = %v after an ordinary error, want active", s.txPhase)
	}
	s.noteStatementOutcome(pgErrorWithCode("23505")) // unique_violation
	if s.txPhase != txActive {
		t.Fatalf("phase = %v after a constraint violation, want active", s.txPhase)
	}

	// 25P02 is the server saying the transaction is finished.
	s.noteStatementOutcome(pgErrorWithCode("25P02"))
	if s.txPhase != txAborted {
		t.Fatalf("phase = %v after 25P02, want aborted", s.txPhase)
	}

	// And it does not un-abort.
	s.noteStatementOutcome(nil)
	if s.txPhase != txAborted {
		t.Errorf("phase = %v, want the abort to be sticky until the boundary", s.txPhase)
	}
}

// Options reach the audit record as what was asked for, not as the caller's
// spelling — the parser has already normalized by then.
func TestDescribeTxOptions(t *testing.T) {
	t.Parallel()

	if got := describeTxOptions(dao.TxOptions{}); got != "server defaults" {
		t.Errorf("default options described as %q", got)
	}
	got := describeTxOptions(dao.TxOptions{Access: dao.TxReadOnly, Isolation: dao.TxSerializable})
	for _, want := range []string{"serializable", "read only"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q does not mention %q", got, want)
		}
	}
}

// AND CHAIN is parsed and refused BY NAME. The ADR §8 rule is that a clause
// is mapped or refused and never silently dropped, and a dropped CHAIN is
// the worst shape of that: the caller believes a new transaction is open.
func TestTxChain_IsRefusedByNameRatherThanDropped(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{"COMMIT AND CHAIN", "ROLLBACK AND CHAIN"} {
		tc, err := ParseTxControl(sql)
		if err != nil {
			t.Fatalf("ParseTxControl(%q): %v", sql, err)
		}
		if !tc.Chain {
			t.Fatalf("%q parsed with Chain=false — the clause was dropped before the engine could refuse it", sql)
		}
	}
	// The refusal itself names the clause and says what to do instead.
	msg := ErrTxChainUnsupported.Error()
	if !strings.Contains(msg, "AND CHAIN") {
		t.Errorf("refusal %q does not name the clause", msg)
	}
}

// The refusal through the ENGINE, not just the parser.
//
// The first version of this test asserted that ParseTxControl sets Chain and
// that the error message names the clause — and it passed with the engine's
// refusal deleted, because it never went near the engine. That is the shape
// the "never silently drop" rule is about, so it is now driven through
// SessionExecute where the drop would actually happen.
func TestTxChain_RefusedThroughTheEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	f.eng.profile = ProfileSession

	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(ctx, f.rootTok, sid, testIP) })

	for _, sql := range []string{"COMMIT AND CHAIN", "ROLLBACK AND CHAIN"} {
		_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, sql, testIP)
		if !errors.Is(err, ErrTxChainUnsupported) {
			t.Errorf("SessionExecute(%q) = %v, want ErrTxChainUnsupported — a dropped CHAIN "+
				"leaves the caller believing a new transaction is open", sql, err)
		}
	}
	// The plain forms still reach the transaction logic rather than being
	// swept up by the chain refusal.
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); !errors.Is(err, ErrNoOpenTx) {
		t.Errorf("bare COMMIT = %v, want ErrNoOpenTx", err)
	}
}

// A session on a driver that cannot pin a transaction across calls is told
// so at BEGIN, not at the first COMMIT — sqlite has no context finalizers,
// which is exactly the case golib's SessionTxBeginner probe exists for.
func TestBeginTx_RefusesADriverThatCannotPin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	f.eng.profile = ProfileSession

	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(ctx, f.rootTok, sid, testIP) })

	_, err = f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP)
	if !errors.Is(err, dao.ErrUnsupported) {
		t.Fatalf("BEGIN on sqlite = %v, want a dao.ErrUnsupported match", err)
	}
	if !strings.Contains(err.Error(), "across calls") {
		t.Errorf("refusal %q should say what the driver cannot do", err)
	}
}

// A COMMIT with nothing open is refused, and the refusal is the engine's own
// rather than something relayed from a server that was never asked.
func TestFinishTx_WithNothingOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	f.eng.profile = ProfileSession
	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(ctx, f.rootTok, sid, testIP) })

	for _, sql := range []string{"COMMIT", "ROLLBACK", "END"} {
		if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, sql, testIP); !errors.Is(err, ErrNoOpenTx) {
			t.Errorf("%s with nothing open = %v, want ErrNoOpenTx", sql, err)
		}
	}
}

// pgErrorWithCode builds a server error carrying a SQLSTATE, so the phase
// transition is driven by the same shape the driver produces.
func pgErrorWithCode(code string) error {
	return &pgconn.PgError{Code: code, Message: "synthetic " + code}
}
