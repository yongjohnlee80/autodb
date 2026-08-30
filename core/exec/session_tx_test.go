package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
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

	// A CLIENT-side failure never reached the server, so it cannot have
	// aborted anything: a guard refusal or a parse error leaves the
	// transaction exactly as it was.
	s.noteStatementOutcome(errors.New("some client-side refusal"))
	if s.txPhase != txActive {
		t.Fatalf("phase = %v after a client-side error, want active", s.txPhase)
	}

	// ANY server error aborts a PostgreSQL transaction — including an
	// ordinary constraint violation. This test previously asserted the
	// opposite, and the live suite disproved it: after a failing statement
	// the NEXT one came back with a raw 25P02, which is the server saying
	// the transaction had been aborted all along. 25P02 is the report, not
	// the event.
	s.noteStatementOutcome(pgErrorWithCode("23505")) // unique_violation
	if s.txPhase != txAborted {
		t.Fatalf("phase = %v after a constraint violation, want aborted — PostgreSQL aborts "+
			"a transaction on ANY error, and waiting for 25P02 marks it one statement late", s.txPhase)
	}

	// And the report itself, on a fresh session.
	s2 := &session{id: "y", txPhase: txActive}
	s2.noteStatementOutcome(pgErrorWithCode("25P02"))
	if s2.txPhase != txAborted {
		t.Fatalf("phase = %v after 25P02, want aborted", s2.txPhase)
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
	useSessionProfile(t, f)

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
	useSessionProfile(t, f)

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
	useSessionProfile(t, f)
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

// useSessionProfile switches the fixture's connection to the session profile
// by writing the column the engine actually reads, rather than by reaching
// into the Engine — so these tests exercise the resolution path a deployment
// uses instead of a shortcut around it.
func useSessionProfile(t *testing.T, f *fixture) {
	t.Helper()

	if err := f.store.Connections.OnCtx(context.Background()).
		With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).
		Update(); err != nil {
		t.Fatalf("switching the connection to the session profile: %v", err)
	}
}

// pgErrorWithCode builds a server error carrying a SQLSTATE, so the phase
// transition is driven by the same shape the driver produces.
func pgErrorWithCode(code string) error {
	return &pgconn.PgError{Code: code, Message: "synthetic " + code}
}

// The profile is resolved from the CONNECTION, with the engine default as
// the fallback and an unrecognized value failing closed (ADR-0074 §2).
func TestProfileFor_ResolvesFromTheConnectionRow(t *testing.T) {
	t.Parallel()

	e := &Engine{profile: ProfileV1Compat}

	// No row, or a row with no profile: the install-wide default.
	if got := e.profileFor(nil); got != ProfileV1Compat {
		t.Errorf("nil row = %q, want the engine default", got)
	}
	if got := e.profileFor(&meta.Connection{}); got != ProfileV1Compat {
		t.Errorf("empty profile = %q, want the engine default", got)
	}
	// The row wins over the default.
	if got := e.profileFor(&meta.Connection{Profile: "session"}); got != ProfileSession {
		t.Errorf("row profile = %q, want session", got)
	}
	// An unrecognized profile is KEPT, not corrected to the default, so
	// admission refuses everything under it. Quietly substituting the
	// default would turn a typo in a connection row into a silent grant of
	// whatever the default permits.
	bogus := e.profileFor(&meta.Connection{Profile: "sesion"})
	if bogus == ProfileV1Compat || bogus == ProfileSession {
		t.Fatalf("a typo resolved to %q — a misconfigured connection must fail closed", bogus)
	}
	st, err := Classify("SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := bogus.admit(st); !errors.Is(err, ErrStatementUnsupported) {
		t.Errorf("admit under an unrecognized profile = %v, want a refusal", err)
	}
}

// A connection carrying the debug flag takes the longer idle bound; every
// other connection takes the short one, which is the safe direction.
func TestConnectionIsDebug_ReadsTheColumn(t *testing.T) {
	t.Parallel()

	if connectionIsDebug(nil) {
		t.Error("a nil row must not read as debug")
	}
	if connectionIsDebug(&meta.Connection{}) {
		t.Error("a connection with the column at its default must not read as debug")
	}
	if !connectionIsDebug(&meta.Connection{Debug: 1}) {
		t.Error("a connection with debug set must read as debug")
	}

	base := txLimits{idleInTx: 90 * time.Second, maxTx: 5 * time.Minute}
	plain := base.forConnection(connectionIsDebug(&meta.Connection{}), 10*time.Minute, time.Hour)
	dbg := base.forConnection(connectionIsDebug(&meta.Connection{Debug: 1}), 10*time.Minute, time.Hour)
	if plain.idleInTx != 90*time.Second {
		t.Errorf("non-debug idle = %s, want 90s", plain.idleInTx)
	}
	if dbg.idleInTx != 10*time.Minute {
		t.Errorf("debug idle = %s, want 10m", dbg.idleInTx)
	}
}

// Existing connections keep v1compat across the migration: enabling sessions
// is a per-connection decision, not something a schema upgrade did for you.
func TestMigration_ExistingConnectionsDefaultToV1Compat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatalf("reading the connection: %v", err)
	}
	if row.Profile != string(ProfileV1Compat) {
		t.Errorf("a freshly created connection has profile %q, want %q — a migration must not "+
			"turn sessions on for connections nobody opted in", row.Profile, ProfileV1Compat)
	}
	if row.IsDebug() {
		t.Error("a freshly created connection reads as debug — it would take the 10-minute idle bound")
	}
	// And it behaves that way: transaction control is refused on it.
	sid, err := f.eng.OpenSession(ctx, f.rootTok, f.connID, testIP)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = f.eng.CloseSession(ctx, f.rootTok, sid, testIP) })
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); !errors.Is(err, ErrStatementUnsupported) {
		t.Errorf("BEGIN on a v1compat connection = %v, want it refused", err)
	}
}
