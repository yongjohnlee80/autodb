package schedmetrics

import "testing"

// DISCARD WITHOUT A RESET IS REPRESENTABLE.
//
// The case the merged single-list design could not express: a backend
// abandoned because its target changed is discarded with a reason and no reset
// was ever attempted, so the reset counter must stay untouched.
func TestDisposition_ADiscardWithNoResetTouchesNoResetCounter(t *testing.T) {
	c := NewDispositionCounts()
	if err := c.NoteRelease(DispositionDiscarded, DiscardTargetChanged); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Reset); n != 0 {
		t.Errorf("the reset counter holds %d entries after a discard that attempted no reset; "+
			"one event has incremented two different things and the totals can no longer "+
			"reconcile", n)
	}
	if c.Where[DispositionDiscarded] != 1 || c.Discards[DiscardTargetChanged] != 1 {
		t.Errorf("the discard was not recorded on both the where and why axes: %+v", c)
	}
	if err := c.Reconcile(); err != nil {
		t.Errorf("reconcile: %v", err)
	}
}

// THE TWO RECONCILIATIONS HOLD ACROSS A REALISTIC MIX.
func TestDisposition_ReconcilesAcrossAMix(t *testing.T) {
	c := NewDispositionCounts()
	// A clean reset, pooled.
	must(t, c.NoteReset(ResetClean))
	must(t, c.NoteRelease(DispositionPooled, DiscardReasonUnset))
	// A failed reset, discarded for that reason.
	must(t, c.NoteReset(ResetFailed))
	must(t, c.NoteRelease(DispositionDiscarded, DiscardResetFailed))
	// No reset at all: shutdown.
	must(t, c.NoteRelease(DispositionDiscarded, DiscardShutdown))
	// Closed outright.
	must(t, c.NoteRelease(DispositionClosed, DiscardReasonUnset))

	if err := c.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c.Where[DispositionDiscarded] != 2 {
		t.Errorf("discards = %d, want 2", c.Where[DispositionDiscarded])
	}
}

// A DISCARD WITHOUT A REASON IS REFUSED.
//
// It would break the second reconciliation, and a discard nobody can explain
// is the row an operator most needs explained.
func TestDisposition_ADiscardNeedsAReason(t *testing.T) {
	c := NewDispositionCounts()
	if err := c.NoteRelease(DispositionDiscarded, DiscardReasonUnset); err == nil {
		t.Fatal("a discard with no reason was accepted; the discard column and the reason " +
			"total would then disagree and nothing would say which was wrong")
	}
	if len(c.Where) != 0 {
		t.Error("the refused release still moved a counter")
	}
}

// AND A REASON ATTACHED TO SOMETHING THAT WAS NOT DISCARDED IS REFUSED.
//
// It asserts something that did not happen, and would inflate the reason total
// past the discard column — the same reconciliation, broken the other way.
func TestDisposition_APooledBackendCannotCarryADiscardReason(t *testing.T) {
	c := NewDispositionCounts()
	if err := c.NoteRelease(DispositionPooled, DiscardResetFailed); err == nil {
		t.Fatal("a pooled backend carried a discard reason")
	}
}

// THE BOUNDED SETS HAVE NO SILENT MEMBERS.
func TestDisposition_EveryValueOutsideTheSetsIsRefused(t *testing.T) {
	c := NewDispositionCounts()
	if err := c.NoteReset(ResetResult(99)); err == nil {
		t.Error("a reset result outside the set was accepted")
	}
	if err := c.NoteRelease(BackendDisposition(99), DiscardReasonUnset); err == nil {
		t.Error("a disposition outside the set was accepted")
	}
	if err := c.NoteRelease(DispositionDiscarded, DiscardReason(99)); err == nil {
		t.Error("a discard reason outside the set was accepted")
	}
	// And the zero values are not labels.
	for _, s := range []string{ResetResultUnset.String(), DispositionUnset.String(), DiscardReasonUnset.String()} {
		if s != "" {
			t.Errorf("a zero value renders as %q, so an unset field could be counted as a real one", s)
		}
	}
}

// EVERY ENUMERATED MEMBER HAS A DISTINCT, NON-EMPTY LABEL.
func TestDisposition_LabelsAreTotalAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	add := func(kind, s string) {
		if s == "" {
			t.Errorf("%s has an empty label", kind)
			return
		}
		if seen[kind+"/"+s] {
			t.Errorf("%s label %q appears twice", kind, s)
		}
		seen[kind+"/"+s] = true
	}
	for _, r := range ResetResults() {
		add("reset", r.String())
	}
	for _, d := range Dispositions() {
		add("disposition", d.String())
	}
	for _, d := range DiscardReasons() {
		add("discard", d.String())
	}
	if len(DiscardReasons()) != 6 {
		t.Errorf("%d discard reasons, want the 6 the bounded set names", len(DiscardReasons()))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
