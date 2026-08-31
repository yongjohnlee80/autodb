package exec

import (
	"context"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Recovery reconciliation — ADR-0074 §7 rev 2.
//
// The reconciler's one rule is that it may only append a terminal it has
// PROVEN. Most of these tests are therefore about what it must NOT do: not
// terminate an entry whose target is merely unreachable, not touch an
// `opened` that no oracle can speak to, not re-probe an entry it just failed
// on. The two live-oracle tests are the positive half.

// seedCrashWindow writes the state a crash between COMMIT and the terminal
// write leaves behind: opened, then commit_started carrying the xid, and no
// terminal.
func seedCrashWindow(t *testing.T, f *fixture, txID string, connID int64, xid string) {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		seq   int64
		state meta.TxState
		xid   string
	}{
		{1, meta.TxOpened, ""},
		{2, meta.TxCommitStarted, xid},
	}
	for _, r := range rows {
		if _, err := f.store.TxOutcomes.OnCtx(ctx).
			Set(meta.TxOutTxID, txID).Set(meta.TxOutSeq, r.seq).
			Set(meta.TxOutState, string(r.state)).Set(meta.TxOutReason, "").
			Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, connID).
			Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, r.xid).
			Set(meta.TxOutCreatedAt, int64(1000+r.seq)).Insert(); err != nil {
			t.Fatalf("seeding the crash window: %v", err)
		}
	}
}

func stateOf(t *testing.T, f *fixture, txID string) TxStatus {
	t.Helper()
	st, err := f.eng.TxOutcome(context.Background(), f.rootTok, txID)
	if err != nil {
		t.Fatalf("TxOutcome(%s): %v", txID, err)
	}
	return st
}

// An unreachable target must leave the entry PENDING.
//
// The most important negative in the file. Terminating here is how a
// committed transaction gets permanently recorded as rolled back: the engine
// would be inferring an outcome from its own inability to ask, which is
// exactly the fabrication §7's invariant forbids.
func TestReconcile_AnUnreachableTargetDoesNotResolveAnything(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// A postgres connection pointing at a port with nothing on it.
	dead, err := f.eng.CreateConnection(ctx, f.rootTok, "dead-target", "postgres",
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	seedCrashWindow(t, f, "tx_unreachable", dead, "424242")

	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Fatalf("resolved %d entries against an unreachable target — it cannot have proven anything", n)
	}
	st := stateOf(t, f, "tx_unreachable")
	if st.Terminal() {
		t.Fatalf("state = %s: an outcome was invented for a target that was never asked", st.State)
	}
	if st.State != meta.TxCommitStarted {
		t.Errorf("state = %s, want commit_started — the entry must stay exactly as it was", st.State)
	}
}

// A dialect with no oracle terminates rather than queueing forever.
//
// MySQL and sqlite have no txid_status, so an indeterminate commit there can
// never be resolved by anyone. Amendment 4 MF2 makes that terminal by
// OUTCOME: leaving it pending would be an unbounded queue of entries that no
// future pass could ever settle.
func TestReconcile_NoOracleDialectTerminatesUnresolvable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// f.connID is the sqlite target from the fixture.
	seedCrashWindow(t, f, "tx_no_oracle", f.connID, "1")

	if n := f.eng.ReconcileOutcomes(ctx); n != 1 {
		t.Fatalf("resolved %d, want 1", n)
	}
	st := stateOf(t, f, "tx_no_oracle")
	if st.State != meta.TxUnresolvable || st.Reason != meta.ReasonNoOracle {
		t.Fatalf("state = %s(%s), want outcome_unresolvable(no-oracle)", st.State, st.Reason)
	}
}

// `opened` with no terminal is NOT the reconciler's business.
//
// A transaction that never reached commit_started cannot have committed, and
// it may be a live session about to commit right now. Terminating it would
// race the boundary handler over a transaction that is doing nothing wrong.
func TestReconcile_LeavesAnOpenedTransactionAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_just_opened", 1, f.connID, meta.TxOpened)
	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Fatalf("resolved %d — an opened transaction was terminated by the reconciler", n)
	}
	if st := stateOf(t, f, "tx_just_opened"); st.State != meta.TxOpened {
		t.Fatalf("state = %s, want opened", st.State)
	}
}

// A failed attempt must not be re-probed on the next pass.
//
// Without the backoff a down database turns every reconciliation pass into a
// fresh connection attempt against it, which is a tight loop aimed at the
// component that is already struggling.
func TestReconcile_BacksOffAfterAFailedAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	dead, err := f.eng.CreateConnection(ctx, f.rootTok, "dead-2", "postgres",
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	seedCrashWindow(t, f, "tx_backoff", dead, "99")

	f.eng.ReconcileOutcomes(ctx)
	if !f.eng.reconcile.claimed("tx_backoff") {
		t.Fatal("no backoff was recorded for a failed attempt")
	}
	// The claim must be refused while the backoff stands, and admitted once
	// it has passed — so the entry is deferred, not abandoned.
	if f.eng.reconcile.claim("tx_backoff", f.eng.now()) {
		t.Error("the entry was re-probed immediately after a failure")
	}
	if !f.eng.reconcile.claim("tx_backoff", f.eng.now().Add(txReconcileBackoff+time.Second)) {
		t.Error("the entry was still refused after the backoff expired — it would never retry")
	}
}

// Reconciling twice resolves nothing the second time, and does not disturb
// the terminal the first pass wrote. Restart idempotence is the §7 gate.
func TestReconcile_IsIdempotentAcrossRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedCrashWindow(t, f, "tx_twice", f.connID, "7")
	if n := f.eng.ReconcileOutcomes(ctx); n != 1 {
		t.Fatalf("first pass resolved %d, want 1", n)
	}
	before := stateOf(t, f, "tx_twice")

	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Fatalf("second pass resolved %d, want 0 — it re-resolved a settled outcome", n)
	}
	after := stateOf(t, f, "tx_twice")
	if before.State != after.State || before.Since != after.Since {
		t.Fatalf("the terminal changed between passes: %s(%s) -> %s(%s)",
			before.State, before.Since, after.State, after.Since)
	}
	rows, err := f.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, "tx_twice").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("log has %d rows after two passes, want 3 — a pass appended a duplicate", len(rows))
	}
}

// --- live oracle ------------------------------------------------------------

// The crash window, resolved from the target's own answer.
//
// A REAL transaction is committed through the engine against live
// PostgreSQL, and its terminal row is then deleted — which is exactly the
// state a crash between tx.Commit() and the outcome write leaves behind. The
// xid is real, the transaction really committed, and txid_status really says
// so. This is the §7 gate: "most crash-window unknowns are thereby
// resolvable".
func TestReconcile_ResolvesARealCrashWindowFromTheTarget(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	for _, sql := range []string{"BEGIN", "INSERT INTO " + table + " (note) VALUES ('survives')", "COMMIT"} {
		if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, sql, testIP); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	var txID string
	var terminalID int64
	for _, r := range rows {
		txID = r.TxID
		if meta.TxState(r.State).IsTerminal() {
			terminalID = r.ID
		}
	}
	if terminalID == 0 {
		t.Fatal("the committed transaction recorded no terminal")
	}
	// Simulate the crash: the COMMIT landed, the outcome write did not.
	if err := f.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutID, terminalID).Delete(); err != nil {
		t.Fatalf("removing the terminal: %v", err)
	}
	if st := stateOf(t, f, txID); st.State != meta.TxCommitStarted {
		t.Fatalf("the seeded crash state is %s, want commit_started", st.State)
	}

	if n := f.eng.ReconcileOutcomes(ctx); n != 1 {
		t.Fatalf("reconciled %d, want 1", n)
	}
	st := stateOf(t, f, txID)
	if st.State != meta.TxCommitted {
		t.Fatalf("state = %s(%s), want committed — the target says this transaction committed, "+
			"and the row it inserted is still there", st.State, st.Reason)
	}
}

// The same window, for a transaction the target ABORTED.
//
// The positive control's mirror: if the reconciler answered "committed" for
// everything it would pass the test above and fail this one. A real
// transaction is opened and rolled back on the live target, and its xid is
// then presented as a crash window.
func TestReconcile_ReportsAnAbortedTransactionAsRolledBack(t *testing.T) {
	f, connID, _, _ := pgSession(t)
	ctx := context.Background()

	connRow, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.eng.target(ctx, connID, connRow)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := target.(dao.SessionTxBeginner)
	if !ok {
		t.Fatal("the live postgres target cannot host a session transaction")
	}
	tx, err := sess.BeginSessionTx(ctx, dao.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	xid := f.eng.captureTargetXID(ctx, tx, "postgres")
	if xid == "" {
		t.Fatal("no xid was assigned to a live transaction")
	}
	if err := tx.RollbackContext(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	seedCrashWindow(t, f, "tx_aborted", connID, xid)
	if n := f.eng.ReconcileOutcomes(ctx); n != 1 {
		t.Fatalf("reconciled %d, want 1", n)
	}
	if st := stateOf(t, f, "tx_aborted"); st.State != meta.TxRolledBack {
		t.Fatalf("state = %s, want rolled_back — the target aborted this transaction", st.State)
	}
}
