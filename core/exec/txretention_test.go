package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Outcome-log retention — ADR-0079 §3 / P4.

// THE ACCEPTANCE CELL (ADR-0079 P4 gate).
//
// Age out a settled transaction, then assert the reader does NOT answer
// ErrNoSuchTx for it. That is the whole invariant: aged-out must stay
// distinguishable from never-existed, because `ErrNoSuchTx` means "no
// transaction was started" and its truthfulness rests on the write-ahead
// ordering (zero rows PROVES nothing started). A retention pass that deleted
// the transaction would make a COMMITTED one answer "no such transaction".
func TestRetention_AnAgedOutTransactionIsNotMistakenForOneThatNeverExisted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_old", 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted)

	n, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("collapsed %d, want 1", n)
	}

	// The decisive assertion.
	st, err := f.eng.TxOutcome(ctx, f.rootTok, "tx_old")
	if errors.Is(err, ErrNoSuchTx) {
		t.Fatal("an aged-out COMMITTED transaction now answers ErrNoSuchTx — a transaction " +
			"that demonstrably happened is being reported as never started")
	}
	if err != nil {
		t.Fatalf("TxOutcome: %v", err)
	}
	if st.State != meta.TxCommitted {
		t.Fatalf("state = %s after collapse, want committed — the outcome itself must survive",
			st.State)
	}

	// And the control: something that really never existed still says so.
	if _, err := f.eng.TxOutcome(ctx, f.rootTok, "tx_never"); !errors.Is(err, ErrNoSuchTx) {
		t.Fatalf("a genuinely absent id returned %v, want ErrNoSuchTx — without this the "+
			"assertion above would be satisfied by a reader that never says not-found", err)
	}
}

// Collapsing prunes the transitions and stamps the survivor, so a tombstone is
// legible as one rather than looking like a short progression.
func TestRetention_CollapseLeavesOneStampedTombstone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_c", 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxRolledBack)
	if _, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0)); err != nil {
		t.Fatal(err)
	}

	rows := txLog(t, f, "tx_c")
	if len(rows) != 1 {
		t.Fatalf("log has %d rows after collapse, want exactly the terminal", len(rows))
	}
	if !meta.TxState(rows[0].State).IsTerminal() {
		t.Fatalf("the surviving row is %q, not a terminal", rows[0].State)
	}
	if rows[0].CollapsedAt == 0 {
		t.Error("the survivor is not stamped, so a collapsed progression cannot be told " +
			"apart from one that only ever had a terminal")
	}
}

// An UNRESOLVED transaction keeps its whole progression.
//
// The reconciler reads `commit_started` to know a COMMIT was in flight, and
// the xid on it to ask the oracle. Pruning that would destroy the recovery
// path the log exists for — so this is the negative that matters most.
func TestRetention_NeverTouchesAnUnresolvedTransaction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedCrashWindow(t, f, "tx_inflight", f.connID, "42")
	if n, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0)); err != nil || n != 0 {
		t.Fatalf("collapsed %d (err %v) — an unresolved transaction was pruned", n, err)
	}
	rows := txLog(t, f, "tx_inflight")
	if len(rows) != 2 {
		t.Fatalf("log has %d rows, want the full progression preserved", len(rows))
	}
	// The oracle input specifically.
	xid := ""
	for _, r := range rows {
		if r.TargetXID != "" {
			xid = r.TargetXID
		}
	}
	if xid == "" {
		t.Fatal("the target xid was pruned — the reconciler can no longer resolve this " +
			"transaction, which is the crash-recovery path the log exists for")
	}
}

// A transaction that settled recently keeps its progression even if it OPENED
// long ago: the whole progression must be inside the window.
func TestRetention_HonoursTheWindowByTheNewestTransition(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// seedTx stamps created_at 1000,1001,1002; collapse with a cutoff that
	// covers the opening but not the terminal.
	seedTx(t, f, "tx_straddle", 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted)
	if n, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(1002, 0)); err != nil || n != 0 {
		t.Fatalf("collapsed %d (err %v) — a transaction whose terminal is outside the "+
			"window was pruned by its opening", n, err)
	}
	if len(txLog(t, f, "tx_straddle")) != 3 {
		t.Fatal("the straddling progression was pruned")
	}
}

// Idempotent: a second pass over the same window is a no-op, not a rewrite.
func TestRetention_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_i", 1, f.connID, meta.TxOpened, meta.TxCommitted)
	if _, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0)); err != nil {
		t.Fatal(err)
	}
	stamp := txLog(t, f, "tx_i")[0].CollapsedAt

	if n, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0)); err != nil || n != 0 {
		t.Fatalf("second pass collapsed %d (err %v), want 0", n, err)
	}
	if got := txLog(t, f, "tx_i")[0].CollapsedAt; got != stamp {
		t.Errorf("the tombstone was re-stamped (%d -> %d); a settled row must not be rewritten",
			stamp, got)
	}
}

// Disabled by default: a zero retention period starts nothing.
func TestStartOutcomeRetention_DisabledByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seedTx(t, f, "tx_untouched", 1, f.connID, meta.TxOpened, meta.TxCommitted)
	f.eng.StartOutcomeRetention(ctx, 10*time.Millisecond, 0) // keep = 0 → off
	time.Sleep(200 * time.Millisecond)

	if len(txLog(t, f, "tx_untouched")) != 2 {
		t.Fatal("retention ran with a zero retention period; it must be opt-in")
	}
	// Positive control: with a period set, the same setup IS collapsed —
	// otherwise "nothing happened" would be satisfied by a pass that never
	// works at all.
	f.eng.StartOutcomeRetention(ctx, 10*time.Millisecond, time.Nanosecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(txLog(t, f, "tx_untouched")) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("retention never ran even with a positive period — the disabled-by-default " +
				"assertion above proves nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// PR #22 r0 MF1: an eligible progression BEHIND a page of tombstones.
//
// A collapsed tombstone still satisfies created_at < cutoff and is skipped as
// having nothing to prune, so a page full of them is a page on which no
// progress is made. With a fixed first page the scan never reaches anything
// behind them and eligible progressions are starved PERMANENTLY, not delayed.
//
// This is the third consumer in one arc to have this shape — the reconciler
// and the history repair sweep both had it (PR #20 r2/r3) — so the cell is
// written the way those two are: fill the first page, put the work behind it.
func TestRetention_ReachesAnEligibleTxBehindAPageOfTombstones(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// A full page of ALREADY-COLLAPSED tombstones, all older than the cutoff.
	for i := 0; i < maxReconcileBatch; i++ {
		id := fmt.Sprintf("tx_tomb_%03d", i)
		if _, err := f.store.TxOutcomes.OnCtx(ctx).
			Set(meta.TxOutTxID, id).Set(meta.TxOutSeq, int64(1)).
			Set(meta.TxOutState, string(meta.TxCommitted)).Set(meta.TxOutReason, "").
			Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, f.connID).
			Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
			Set(meta.TxOutCreatedAt, int64(10)).Set(meta.TxOutCollapsedAt, int64(11)).
			Insert(); err != nil {
			t.Fatal(err)
		}
	}
	// The eligible one, inserted last so it sits behind all of them.
	seedTx(t, f, "tx_behind", 1, f.connID, meta.TxOpened, meta.TxCommitted)

	n, err := f.eng.CollapseSettledOutcomes(ctx, time.Unix(9999, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("collapsed %d, want 1 — the scan never got past a page of tombstones, "+
			"so a transaction behind them is starved permanently rather than delayed", n)
	}
	if got := len(txLog(t, f, "tx_behind")); got != 1 {
		t.Fatalf("tx_behind still has %d rows; it was never reached", got)
	}
}

// A tombstone is excluded at the QUERY, not skipped in the loop. Otherwise it
// consumes page budget forever for work that will never be done.
func TestRetention_TombstonesDoNotConsumePageBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := f.store.TxOutcomes.OnCtx(ctx).
			Set(meta.TxOutTxID, fmt.Sprintf("tx_done_%d", i)).Set(meta.TxOutSeq, int64(1)).
			Set(meta.TxOutState, string(meta.TxCommitted)).Set(meta.TxOutReason, "").
			Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, f.connID).
			Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
			Set(meta.TxOutCreatedAt, int64(10)).Set(meta.TxOutCollapsedAt, int64(11)).
			Insert(); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.store.TxOutcomes.OnCtx(ctx).
		WithPredicate(dao.Eq(string(meta.TxOutCollapsedAt), int64(0))).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the un-collapsed predicate matched %d tombstones", len(rows))
	}
}
