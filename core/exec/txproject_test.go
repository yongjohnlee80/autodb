package exec

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// History as a projection of the outcome log — ADR-0074 §7 rev 2.

func histStatus(t *testing.T, f *fixture, txID string) []HistStatus {
	t.Helper()
	rows, err := f.store.History.OnCtx(context.Background()).With(meta.HistTxID, txID).Select()
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	out := make([]HistStatus, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Status)
	}
	return out
}

func seedHistory(t *testing.T, f *fixture, txID string, status HistStatus) int64 {
	t.Helper()
	id, err := f.store.History.OnCtx(context.Background()).
		Set(meta.HistUserID, int64(1)).Set(meta.HistConnID, f.connID).
		Set(meta.HistIP, testIP).Set(meta.HistScript, "INSERT INTO t VALUES (1)").
		Set(meta.HistStartedAt, int64(1)).Set(meta.HistDurationMS, int64(1)).
		Set(meta.HistRowCount, int64(1)).Set(meta.HistStatus, status).
		Set(meta.HistError, "").Set(meta.HistTxID, txID).Insert()
	if err != nil {
		t.Fatalf("seeding history: %v", err)
	}
	return id
}

// A terminal settles the statements that were pending on it.
func TestResolveHistory_TheTerminalSettlesItsStatements(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedHistory(t, f, "tx_h", StatusPendingCommit)
	seedHistory(t, f, "tx_h", StatusPendingCommit)

	if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_h", state: meta.TxOpened}); err != nil {
		t.Fatal(err)
	}
	for _, s := range histStatus(t, f, "tx_h") {
		if s != StatusPendingCommit {
			t.Fatalf("status = %q before the boundary, want ok_pending_commit", s)
		}
	}

	if err := f.eng.appendTxOutcome(ctx, txTransition{txID: "tx_h", state: meta.TxCommitted}); err != nil {
		t.Fatal(err)
	}
	for _, s := range histStatus(t, f, "tx_h") {
		if s != StatusOK {
			t.Fatalf("status = %q after a commit, want ok", s)
		}
	}
}

// A rolled-back transaction's statements say so, rather than reverting to a
// bare "ok" that would claim an effect the target discarded.
func TestResolveHistory_ARollbackIsNotAnOK(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedHistory(t, f, "tx_rb", StatusPendingCommit)
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_rb", state: meta.TxRolledBack, reason: meta.ReasonTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	if got := histStatus(t, f, "tx_rb"); len(got) != 1 || got[0] != StatusRolledBack {
		t.Fatalf("status = %v, want [rolled_back]", got)
	}
}

// A statement that FAILED keeps its own error.
//
// The statement failing and the transaction rolling back are different
// facts. Overwriting the first with the second would erase the only record
// of WHY the transaction went the way it did — and it is the same row that
// would have to answer "which statement broke this?".
func TestResolveHistory_AFailedStatementKeepsItsOwnStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedHistory(t, f, "tx_mixed", StatusError)
	seedHistory(t, f, "tx_mixed", StatusPendingCommit)

	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_mixed", state: meta.TxRolledBack,
	}); err != nil {
		t.Fatal(err)
	}
	got := histStatus(t, f, "tx_mixed")
	var errors, rolled int
	for _, s := range got {
		switch s {
		case StatusError:
			errors++
		case StatusRolledBack:
			rolled++
		}
	}
	if errors != 1 || rolled != 1 {
		t.Fatalf("statuses = %v, want one error preserved and one rolled_back", got)
	}
}

// An unresolvable outcome is projected as such, not as ok and not as error.
// The statement did not fail, and no future pass will improve on the answer.
func TestResolveHistory_UnresolvableIsItsOwnStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	seedHistory(t, f, "tx_unres", StatusPendingCommit)
	if err := f.eng.appendTxOutcome(ctx, txTransition{
		txID: "tx_unres", state: meta.TxUnresolvable, reason: meta.ReasonNoOracle,
	}); err != nil {
		t.Fatal(err)
	}
	if got := histStatus(t, f, "tx_unres"); len(got) != 1 || got[0] != StatusUnresolvable {
		t.Fatalf("status = %v, want [outcome_unresolvable]", got)
	}
}

// Autocommit work is unaffected: no tx_id, so nothing to defer and nothing
// to project. The control that keeps the pending status from leaking onto
// every statement in the system.
func TestRecordOutcome_AutocommitStaysOK(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.eng.Execute(ctx, f.rootTok, f.connID,
		"CREATE TABLE t (id INTEGER PRIMARY KEY)", testIP); err != nil {
		t.Fatal(err)
	}
	rows, err := f.eng.ListHistory(ctx, f.rootTok, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no history recorded")
	}
	for _, r := range rows {
		if r.Status != string(StatusOK) {
			t.Errorf("autocommit statement status = %q, want ok — its effect is durable on return",
				r.Status)
		}
	}
}

// The whole point, end to end on a live target: an in-transaction statement
// is PENDING while the transaction is open, and only becomes ok when the
// commit says so.
//
// The old code wrote "ok" the moment the statement returned, so a reader
// looking at history mid-transaction saw durable success for work that a
// rollback would discard — and after a crash, for work that may never have
// landed at all. This is that defect's regression cell.
func TestSessionTx_AnInTransactionStatementIsPendingUntilTheCommit(t *testing.T) {
	f, _, sid, table := pgSession(t)
	ctx := context.Background()

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid,
		"INSERT INTO "+table+" (note) VALUES ('pending')", testIP); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Read history WHILE the transaction is still open.
	rows, err := f.eng.ListHistory(ctx, f.rootTok, 50)
	if err != nil {
		t.Fatal(err)
	}
	var insert *HistoryRow
	for i := range rows {
		if rows[i].Script == "INSERT INTO "+table+" (note) VALUES ('pending')" {
			insert = &rows[i]
		}
	}
	if insert == nil {
		t.Fatal("the in-transaction INSERT was not recorded at all")
	}
	if insert.Status != string(StatusPendingCommit) {
		t.Fatalf("status mid-transaction = %q, want ok_pending_commit — a reader would see "+
			"durable success for work a rollback would discard", insert.Status)
	}

	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "COMMIT", testIP); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	rows, err = f.eng.ListHistory(ctx, f.rootTok, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == insert.ID && r.Status != string(StatusOK) {
			t.Fatalf("status after COMMIT = %q, want ok — the projection never resolved", r.Status)
		}
	}
}
