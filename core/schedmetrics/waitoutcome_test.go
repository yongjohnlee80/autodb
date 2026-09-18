package schedmetrics

// THE LABEL SET IS TOTAL OVER THE TYPED RESULT, AND CLOSED.
//
// The guard that matters is not "the labels are right". It is that an outcome
// added to core/exec cannot become reachable, real and uncounted while every
// cell here still passes — which is the drift the design was written against.

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// EVERY DECLARED OUTCOME HAS A DISTINCT, NON-EMPTY LABEL.
func TestWaitLabels_TotalAndDistinct(t *testing.T) {
	outs := exec.WaitOutcomes()
	if len(outs) == 0 {
		t.Fatal("core/exec declares no wait outcomes; this cell would pass no matter what " +
			"the measurement side did, so the reading is not evidence")
	}
	seen := map[string]exec.WaitOutcome{}
	for _, o := range outs {
		l, ok := Label(o)
		if !ok || l == "" {
			t.Errorf("outcome %d has no label; a producible outcome nothing can name is one "+
				"the breakdown loses while the total still looks right", o)
			continue
		}
		if prev, dup := seen[l]; dup {
			t.Errorf("outcomes %d and %d share the label %q; two outcomes under one series "+
				"cannot be told apart by the person reading it", prev, o, l)
		}
		seen[l] = o
	}
	if got := len(WaitLabels()); got != len(outs) {
		t.Errorf("WaitLabels() has %d entries for %d outcomes", got, len(outs))
	}
}

// THE ZERO VALUE IS NOT A LABEL.
//
// It is what an unreported wait would carry, and giving it a name would let
// "nobody said" be counted as a real outcome.
func TestWaitLabels_TheZeroValueHasNone(t *testing.T) {
	if l, ok := Label(exec.WaitOutcomeUnset); ok || l != "" {
		t.Fatalf("the unset outcome has the label %q; an outcome nobody named would be "+
			"counted as though somebody had", l)
	}
}

// THE COMPLETENESS GUARD, AND THE ONE THIS FILE EXISTS FOR.
//
// The failure it defends against: somebody adds a constant to the outcome
// block and a String() case for it, wires a raise site, and never adds it to
// WaitOutcomes(). The outcome is then produced and labelled, and every cell
// above still passes — because they all iterate WaitOutcomes(), which is the
// list that was not updated.
//
// So this does NOT iterate that list. It scans the value space and requires
// every value that has a label to be declared, which is the only direction
// that catches the omission.
func TestWaitLabels_NoLabelledOutcomeIsUndeclared(t *testing.T) {
	declared := map[exec.WaitOutcome]bool{}
	for _, o := range exec.WaitOutcomes() {
		declared[o] = true
	}
	// The span is deliberately far wider than the enum: the point is to find a
	// value somebody added, and a bound of len(declared) would stop exactly
	// where the missing one lives.
	for v := 0; v < 64; v++ {
		o := exec.WaitOutcome(v)
		l, ok := Label(o)
		if !ok {
			continue
		}
		if o == exec.WaitOutcomeUnset {
			t.Errorf("the unset value carries the label %q", l)
			continue
		}
		if !declared[o] {
			t.Errorf("exec.WaitOutcome(%d) has the label %q but is not in WaitOutcomes(); it "+
				"can be produced and cannot be enumerated, so every total computed from the "+
				"declared list is short by however often it happens", v, l)
		}
	}
}
