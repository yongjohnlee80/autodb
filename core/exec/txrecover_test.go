package exec

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// PR #20 r0 MF1, MF2, MF3, MF5 — the review's four structural findings.

// --- MF1: a prior-process `opened` row reaches a terminal ------------------

// A crash after `opened` and before `commit_started` leaves a row with no
// later owner: the dead process's session, timeout reaper and boundary
// handler all went with it. Nothing would ever settle it, and §7's
// exactly-one-terminal would be false for that transaction forever.
func TestRecoverStaleOpen_SettlesATransactionInheritedFromADeadProcess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_orphan", 1, f.connID, meta.TxOpened)
	seedHistory(t, f, "tx_orphan", StatusPendingCommit)

	// The ordinary reconciler must NOT touch it: while a session is live,
	// `opened` is a healthy transaction about to commit.
	if n := f.eng.ReconcileOutcomes(ctx); n != 0 {
		t.Fatalf("the periodic pass settled %d opened transaction(s); that races a live commit", n)
	}

	if n := f.eng.RecoverStaleOpen(ctx); n != 1 {
		t.Fatalf("startup recovery settled %d, want 1", n)
	}
	st := stateOf(t, f, "tx_orphan")
	if st.State != meta.TxRolledBack {
		t.Fatalf("state = %s, want rolled_back — it never reached commit_started, so it "+
			"cannot have committed", st.State)
	}
	// The surface has to follow, or the outcome is settled somewhere nobody
	// looks.
	if got := histStatus(t, f, "tx_orphan"); len(got) != 1 || got[0] != StatusRolledBack {
		t.Fatalf("history = %v, want [rolled_back]", got)
	}
	// Idempotent across restarts.
	if n := f.eng.RecoverStaleOpen(ctx); n != 0 {
		t.Errorf("a second startup settled %d already-settled transaction(s)", n)
	}
}

// The safety argument is an ORDERING one: at startup this process has no
// sessions, so every `opened` row belongs to a dead process. If that does not
// hold, the sweep must refuse rather than roll back live work.
func TestRecoverStaleOpen_RefusesToRunOnceSessionsExist(t *testing.T) {
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil || len(rows) == 0 {
		t.Fatalf("no opened row for the live transaction: %v", err)
	}
	txID := rows[0].TxID

	if n := f.eng.RecoverStaleOpen(ctx); n != 0 {
		t.Fatalf("startup recovery settled %d transaction(s) while a session was open — "+
			"it rolled back live work", n)
	}
	if st := stateOf(t, f, txID); st.State != meta.TxOpened {
		t.Fatalf("state = %s, want opened — a live transaction was terminated", st.State)
	}
	// Positive control: it still commits normally afterwards.
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); err != nil {
		t.Fatalf("COMMIT after the sweep declined: %v", err)
	}
}

// --- MF2: the trail ENDS in its terminal -----------------------------------

// The store's partial index refuses a second terminal but permits a
// nonterminal after one. That gap is reachable: the reconciler can resolve a
// durable commit_started while finishTx is between the target's answer and
// its own append.
func TestAppendTxOutcome_ATerminalEndsTheTrailForEveryAppender(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for _, st := range []meta.TxState{meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted} {
		if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_end", state: st}); err != nil {
			t.Fatal(err)
		}
	}
	// A late nonterminal — finishTx classifying the driver's answer as
	// unknown after the reconciler already resolved it.
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_end", state: meta.TxUnknownPending, reason: meta.ReasonUnanswered,
	}); err != nil {
		t.Fatalf("a late nonterminal must be a no-op, not an error: %v", err)
	}

	rows := txLog(t, f, "tx_end")
	if len(rows) != 3 {
		t.Fatalf("the trail grew to %d rows; an append-only trail must END in its terminal", len(rows))
	}
	var last *meta.TxOutcome
	for _, r := range rows {
		if last == nil || r.Seq > last.Seq {
			last = r
		}
	}
	if !meta.TxState(last.State).IsTerminal() {
		t.Fatalf("the last transition is %q, not a terminal", last.State)
	}
}

// The same property through the REAL concurrent window rather than
// sequentially: the reconciler resolves the transaction while the boundary
// handler is between the target's answer and its own append.
func TestCommitBoundary_AResolverWinningTheRaceLeavesTheTrailEndingInItsTerminal(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"INSERT INTO "+table+" (note) VALUES ('raced')", testIP); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Injected window: the instant the target's COMMIT has returned and
	// before the boundary writes its own outcome, another resolver settles
	// the transaction. This is the real interleaving, not a simulation of it.
	var raced bool
	txBoundaryHook = func(p txBoundaryPoint) {
		if p != boundaryCommitReturned || raced {
			return
		}
		raced = true
		rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
		if err != nil || len(rows) == 0 {
			t.Errorf("no outcome rows inside the window: %v", err)
			return
		}
		if err := f.eng.appendTxOutcome(ctx, txTransition{
			txID: rows[0].TxID, state: meta.TxCommitted, connectionID: f.connID,
		}); err != nil {
			t.Errorf("the competing resolver could not append: %v", err)
		}
	}
	t.Cleanup(func() { txBoundaryHook = nil })

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if !raced {
		t.Fatal("the injected window never fired — the test proves nothing")
	}

	rows, err := f.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	var last *meta.TxOutcome
	terminals := 0
	for _, r := range rows {
		if last == nil || r.Seq > last.Seq {
			last = r
		}
		if meta.TxState(r.State).IsTerminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("%d terminals for one transaction, want exactly 1", terminals)
	}
	if !meta.TxState(last.State).IsTerminal() {
		t.Fatalf("the boundary appended %q AFTER the resolver's terminal — the trail no "+
			"longer ends in its outcome", last.State)
	}
}

// --- MF3: the terminal and its projection cannot separate ------------------

// A crash between the terminal write and the history projection used to leave
// the truth terminal and the surface pending forever, because reconciliation
// folds the terminal and skips the group.
func TestReconcile_RepairsHistoryLeftPendingUnderASettledTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// Exactly the state a death between the two writes leaves behind.
	seedTx(t, f, "tx_split", 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted)
	seedHistory(t, f, "tx_split", StatusPendingCommit)

	f.eng.ReconcileOutcomes(ctx)
	if got := histStatus(t, f, "tx_split"); len(got) != 1 || got[0] != StatusOK {
		t.Fatalf("history = %v after recovery, want [ok] — the outcome is settled and the "+
			"surface still says pending, which is exactly what nobody would ever fix", got)
	}
}

// And the window is closed at the source: the terminal and its projection are
// one meta transaction, so a failure in either leaves NEITHER.
func TestAppendTxOutcome_TheTerminalAndItsProjectionAreAtomic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedTx(t, f, "tx_atomic", 1, f.connID, meta.TxOpened)
	seedHistory(t, f, "tx_atomic", StatusPendingCommit)

	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_atomic", state: meta.TxCommitted, connectionID: f.connID,
	}); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f, "tx_atomic"); st.State != meta.TxCommitted {
		t.Fatalf("state = %s, want committed", st.State)
	}
	if got := histStatus(t, f, "tx_atomic"); len(got) != 1 || got[0] != StatusOK {
		t.Fatalf("history = %v, want [ok] — the projection did not land with the terminal", got)
	}
}

// --- MF5: the ratified interval semantics ----------------------------------

// Amendment 4 A1: unset takes the default, zero or negative DISABLES the
// periodic pass, leaving startup and checkout reconciliation. Rejecting a
// non-positive value made the ratified configuration unreachable.
func TestStartOutcomeReconciler_NonPositiveIntervalDisablesOnlyTheTicker(t *testing.T) {
	t.Parallel()

	for _, every := range []time.Duration{0, -time.Second} {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// A stale opened row and a no-oracle pending: STARTUP must settle
		// both even with the ticker disabled.
		seedTx(t, f, "tx_startup", 1, f.connID, meta.TxOpened)
		seedCrashWindow(t, f, "tx_pending", f.connID, "5")

		f.eng.StartOutcomeReconciler(ctx, every)

		if st := stateOf(t, f, "tx_startup"); !st.Terminal() {
			t.Errorf("interval %s: startup recovery did not run; state = %s", every, st.State)
		}
		if st := stateOf(t, f, "tx_pending"); !st.Terminal() {
			t.Errorf("interval %s: the startup oracle pass did not run; state = %s", every, st.State)
		}

		// And the ticker really is off. Seeded AFTER startup, so only a
		// periodic pass could settle it — without this the test would be
		// satisfied by a scheduler that ignored the setting entirely and
		// ticked anyway.
		seedCrashWindow(t, f, "tx_after_startup", f.connID, "6")
		time.Sleep(300 * time.Millisecond)
		if st := stateOf(t, f, "tx_after_startup"); st.Terminal() {
			t.Errorf("interval %s: an entry seeded after startup was settled, so the periodic "+
				"pass is still running despite being disabled", every)
		}
	}
}

// The positive control for the cell above: with a positive interval, an entry
// seeded after startup IS settled by the ticker. Without this, "it stayed
// pending" would be satisfied by a reconciler that never worked at all.
func TestStartOutcomeReconciler_APositiveIntervalKeepsTicking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.eng.StartOutcomeReconciler(ctx, 20*time.Millisecond)
	seedCrashWindow(t, f, "tx_ticked", f.connID, "7")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if stateOf(t, f, "tx_ticked").Terminal() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an entry seeded after startup was never settled: the periodic pass is not running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The checkout trigger is what keeps a disabled ticker from stranding
// entries: a pending outcome resolves the next time its own connection is
// used.
func TestReconcileConnection_ACheckoutResolvesThatConnectionsBacklog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedCrashWindow(t, f, "tx_checkout", f.connID, "11")

	// Scoped: another connection's checkout must not resolve this one.
	other, err := f.eng.CreateConnection(ctx, f.rootTok, "other", "sqlite",
		"file:other?mode=memory&cache=shared", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.eng.ReconcileConnection(ctx, other); n != 0 {
		t.Fatalf("a checkout on connection %d resolved %d entries belonging to another", other, n)
	}
	if st := stateOf(t, f, "tx_checkout"); st.Terminal() {
		t.Fatal("an unrelated connection's checkout settled this transaction")
	}

	if n := f.eng.ReconcileConnection(ctx, f.connID); n != 1 {
		t.Fatalf("the owning connection's checkout resolved %d, want 1", n)
	}
	if st := stateOf(t, f, "tx_checkout"); !st.Terminal() {
		t.Fatalf("state = %s, want a terminal", st.State)
	}
}

// --- r1 MF1: the pending query must be SELECTIVE ---------------------------

// A backlog query must return the backlog, not the history.
//
// The previous version selected rows whose state was opened, commit_started
// or unknown_pending — and every transaction keeps its `opened` row forever,
// and every committed one keeps `commit_started` too. No predicate over
// states can separate pending from settled, so the "candidate" set was every
// transaction ever recorded: lector's probe seeded 32 settled groups plus one
// pending and got all 33 back.
//
// That is not a slow query, it is a broken one. It reloads the whole log
// through a growing IN list, and when that list reaches the driver's
// parameter limit reconciliation does not degrade — it stops, silently, at
// exactly the moment the backlog is largest.
func TestPendingGroups_ReturnsOnlyTheUnresolvedBacklog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	const settled = 32
	for i := 0; i < settled; i++ {
		id := fmt.Sprintf("tx_settled_%02d", i)
		seedTx(t, f, id, 1, f.connID, meta.TxOpened, meta.TxCommitStarted, meta.TxCommitted)
	}
	seedCrashWindow(t, f, "tx_really_pending", f.connID, "77")

	groups, err := f.eng.pendingGroups(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		var ids []string
		for id := range groups {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Fatalf("pendingGroups returned %d groups for a backlog of 1: %v\n"+
			"a settled transaction still has its opened and commit_started rows, so a "+
			"state predicate selects the entire history", len(groups), ids)
	}
	if _, ok := groups["tx_really_pending"]; !ok {
		t.Fatal("the one genuinely pending transaction was not returned")
	}

	// The queue is the reason, and it must shrink as transactions settle.
	n, err := f.store.TxPending.OnCtx(ctx).Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the pending queue holds %d rows for a backlog of 1 — settled transactions "+
			"are not leaving it", n)
	}
}

// Settling a transaction removes it from the queue, in the same store
// transaction as the terminal. An entry that outlived its terminal would be
// probed forever; one deleted before the terminal landed would be lost.
func TestPendingQueue_ATerminalDequeuesAtomically(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_q", state: meta.TxOpened, connectionID: f.connID,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.store.TxPending.OnCtx(ctx).With(meta.TxPendTxID, "tx_q").Count(); n != 1 {
		t.Fatalf("an opened transaction is not in the queue (%d rows); nothing would ever "+
			"come back for it", n)
	}
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_q", state: meta.TxCommitted, connectionID: f.connID,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.store.TxPending.OnCtx(ctx).With(meta.TxPendTxID, "tx_q").Count(); n != 0 {
		t.Fatalf("a settled transaction is still queued (%d rows) and would be probed forever", n)
	}
}

// The bounded sweep heals rows stranded under a settled outcome, and is
// driven from HISTORY — the strandable rows are exactly the ones still marked
// pending, and there are normally none. Walking settled transactions to find
// them cost one query per transaction ever recorded, per pass.
func TestRepairPendingHistory_HealsWithoutWalkingEveryTransaction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("tx_ok_%02d", i)
		seedTx(t, f, id, 1, f.connID, meta.TxOpened, meta.TxCommitted)
		seedHistory(t, f, id, StatusOK)
	}
	// One legacy strand: settled outcome, surface still pending.
	seedTx(t, f, "tx_stranded", 1, f.connID, meta.TxOpened, meta.TxCommitted)
	seedHistory(t, f, "tx_stranded", StatusPendingCommit)

	f.eng.repairPendingHistory(ctx)

	if got := histStatus(t, f, "tx_stranded"); len(got) != 1 || got[0] != StatusOK {
		t.Fatalf("history = %v, want [ok] — the stranded row was not healed", got)
	}
	// A row whose transaction is genuinely still open must NOT be touched.
	seedCrashWindow(t, f, "tx_inflight", f.connID, "3")
	seedHistory(t, f, "tx_inflight", StatusPendingCommit)
	f.eng.repairPendingHistory(ctx)
	if got := histStatus(t, f, "tx_inflight"); len(got) != 1 || got[0] != StatusPendingCommit {
		t.Fatalf("history = %v, want [ok_pending_commit] — an in-flight statement was "+
			"resolved by a repair pass", got)
	}
}

// Engine-owned background work must not outlive Close.
//
// The checkout trigger uses the very pools Close tears down, so a detached
// goroutine could reopen one that had just been shut. Close cancels it and
// WAITS before touching any pool (PR #20 r1 SF).
func TestEngineClose_StopsAndWaitsForCheckoutReconciliation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedCrashWindow(t, f, "tx_bg", f.connID, "42")
	// A checkout: this is what fires the trigger on the real path.
	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID,
		"CREATE TABLE bg (id INTEGER PRIMARY KEY)", testIP); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close returned, so every engine-owned goroutine has finished. If any
	// were still running it would be touching a closed pool right now, and
	// -race would say so.
	if f.eng.bgCtx.Err() == nil {
		t.Fatal("the engine's background context is still live after Close")
	}
	// And nothing reopened a pool behind Close's back.
	f.eng.mu.Lock()
	open := len(f.eng.conns)
	f.eng.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d pool(s) open after Close — background work reopened one", open)
	}
}
