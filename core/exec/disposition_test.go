package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// disposition_test.go holds the terminal write's rules: one disposition per
// attempt, a repeat is a no-op, a different one is a conflict, and the
// disposition is about the ATTEMPT — never read off the effect's status.

func histOf(t *testing.T, f *fixture, att Attempt) *meta.HistoryEntry {
	t.Helper()
	row, err := f.store.History.OnCtx(context.Background()).With(meta.HistID, att.HistID).Get()
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func attemptFor(t *testing.T, f *fixture, txID string) Attempt {
	t.Helper()
	att, err := f.eng.recordAttempt(context.Background(), rootIdent(t, f), f.connID, testIP, "SELECT 1", txID)
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func TestTheSameTerminalTwiceIsOneTerminal(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	att := attemptFor(t, f, "")
	if err := f.eng.writeOutcome(ctx, rootIdent(t, f), f.connID, testIP, att, 5*time.Millisecond, 1, StatusOK, "", ""); err != nil {
		t.Fatal(err)
	}
	// A retry of the same recording, re-measured: accepted, and changes nothing.
	if err := f.eng.writeOutcome(ctx, rootIdent(t, f), f.connID, testIP, att, 9*time.Millisecond, 7, StatusOK, "", ""); err != nil {
		t.Fatalf("an identical repeat: %v, want nil", err)
	}
	h := histOf(t, f, att)
	if h.Disposition != meta.DispositionCompleted || h.DurationMS != 5 || h.RowCount != 1 {
		t.Errorf("after the repeat: disposition %q, %dms, %d rows; want completed, 5ms, 1 row (the first write stands)",
			h.Disposition, h.DurationMS, h.RowCount)
	}
	if n := f.auditCount(t, "exec_result"); n != 1 {
		t.Errorf("%d exec_result rows for one attempt, want 1", n)
	}
}

// The case status could not answer: the transaction's commit refines status
// AFTER the first terminal, and a retry of that terminal is still the same
// delivery.
func TestARepeatAfterTheCommitProjectedIsStillTheSameTerminal(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	att := attemptFor(t, f, "tx_projected")
	id := rootIdent(t, f)
	if err := f.eng.writeOutcome(ctx, id, f.connID, testIP, att, 0, 1, StatusPendingCommit, "", "tx_projected"); err != nil {
		t.Fatal(err)
	}
	f.eng.resolveHistory(ctx, "tx_projected", meta.TxCommitted)
	if h := histOf(t, f, att); h.Status != StatusOK || h.Disposition != meta.DispositionCompleted {
		t.Fatalf("after the commit: status %q disposition %q, want ok and completed", h.Status, h.Disposition)
	}
	if err := f.eng.writeOutcome(ctx, id, f.connID, testIP, att, 0, 1, StatusPendingCommit, "", "tx_projected"); err != nil {
		t.Fatalf("the identical terminal, retried after the projection: %v, want nil", err)
	}
	if h := histOf(t, f, att); h.Status != StatusOK {
		t.Errorf("the retry rewrote the projected status to %q", h.Status)
	}
}

func TestADifferentTerminalIsAConflictAndNothingIsOverwritten(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	att := attemptFor(t, f, "")
	id := rootIdent(t, f)
	if err := f.eng.writeOutcome(ctx, id, f.connID, testIP, att, 0, 1, StatusOK, "", ""); err != nil {
		t.Fatal(err)
	}
	err := f.eng.writeOutcome(ctx, id, f.connID, testIP, att, 0, 0, StatusError, "boom", "")
	if !errors.Is(err, ErrDispositionConflict) {
		t.Fatalf("a failed terminal after a completed one: %v, want ErrDispositionConflict", err)
	}
	if h := histOf(t, f, att); h.Disposition != meta.DispositionCompleted || h.Status != StatusOK {
		t.Errorf("after the conflict: %q/%q, want the first terminal, completed/ok", h.Disposition, h.Status)
	}
	c := f.audits(t, "disposition_conflict")
	if len(c) != 1 || c[0].AttemptID != att.ID || c[0].ConnID != f.connID {
		t.Errorf("disposition_conflict rows %+v, want one naming the attempt and connection", c)
	}
}

// The projection moves status, never the disposition.
func TestTheProjectionNeverTouchesTheDisposition(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	att := attemptFor(t, f, "tx_rolled")
	if err := f.eng.writeOutcome(ctx, rootIdent(t, f), f.connID, testIP, att, 0, 1, StatusPendingCommit, "", "tx_rolled"); err != nil {
		t.Fatal(err)
	}
	f.eng.resolveHistory(ctx, "tx_rolled", meta.TxRolledBack)
	if h := histOf(t, f, att); h.Status != StatusRolledBack || h.Disposition != meta.DispositionCompleted {
		t.Errorf("after the rollback: status %q disposition %q, want rolled_back and completed — the statement ran; its effect is what went",
			h.Status, h.Disposition)
	}
}

// The write is keyed by the history row AND the attempt id, so two rows that
// share an attempt id (128 random bits: forced here) are two rows.
func TestACollidingAttemptIDChangesOneRow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a, b := attemptFor(t, f, ""), attemptFor(t, f, "")
	if err := f.store.History.OnCtx(ctx).With(meta.HistID, b.HistID).Set(meta.HistAttemptID, a.ID).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.writeOutcome(ctx, rootIdent(t, f), f.connID, testIP, a, 0, 1, StatusOK, "", ""); err != nil {
		t.Fatal(err)
	}
	if h := histOf(t, f, Attempt{HistID: b.HistID}); h.Disposition.IsSet() {
		t.Errorf("the other row sharing the attempt id became %q; one write changed two rows", h.Disposition)
	}
}

func TestATerminalForNoSuchAttemptIsAWriterBug(t *testing.T) {
	f := newFixture(t)
	err := f.eng.writeOutcome(context.Background(), rootIdent(t, f), f.connID, testIP,
		Attempt{ID: newAttemptID(), HistID: 1 << 40}, 0, 0, StatusOK, "", "")
	if !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("a terminal for an attempt with no row: %v, want ErrNoAttempt", err)
	}
}

// A refusal is its own disposition; status keeps what every reader before
// dispositions saw.
func TestARefusalIsDispositionRefusedWithStatusError(t *testing.T) {
	f := newFixture(t)
	att := attemptFor(t, f, "")
	_ = f.eng.rejectRecordedAttempt(context.Background(), rootIdent(t, f), f.connID, testIP, "SELECT 1", att, errRefusedForTest)
	if h := histOf(t, f, att); h.Disposition != meta.DispositionRefused || h.Status != StatusError {
		t.Errorf("a refused attempt: %q/%q, want refused/error", h.Disposition, h.Status)
	}
}

// The ordinary paths map as the table says, and the effect's
// outcome_unresolvable is completed or unknown by what was OBSERVED.
func TestDispositionForReadsTheAttemptNotTheEffect(t *testing.T) {
	for _, tc := range []struct {
		status   HistStatus
		observed bool
		want     meta.Disposition
	}{
		{StatusOK, true, meta.DispositionCompleted},
		{StatusPendingCommit, true, meta.DispositionCompleted},
		{StatusError, true, meta.DispositionFailed},
		{StatusRolledBack, true, meta.DispositionRolledBack},
		{StatusUnresolvable, true, meta.DispositionCompleted},
		{StatusUnresolvable, false, meta.DispositionUnknown},
		{StatusUnknown, false, meta.DispositionUnknown},
	} {
		got, err := dispositionFor(tc.status, tc.observed)
		if err != nil || got != tc.want {
			t.Errorf("dispositionFor(%s, observed %v) = %q, %v; want %q", tc.status, tc.observed, got, err, tc.want)
		}
	}
	if _, err := dispositionFor(StatusRunning, true); err == nil {
		t.Error("running was accepted as a terminal")
	}
}
