package schedmetrics

// THE LABEL SET IS CLOSED, AND THESE CELLS ARE WHAT KEEPS IT CLOSED.
//
// The design's requirement is that adding an outcome without a mapping fails
// the build and that no fallback bucket exists. Go has no exhaustiveness check
// for a uint8 enum, so the guard has to be a cell — and the cell that matters
// is not "the labels are right" but "a NEW sentinel in core/exec cannot pass
// unnoticed", which is the drift the design was written against.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// EVERY PRODUCIBLE OUTCOME CLASSIFIES, AND TO THE RIGHT LABEL.
func TestClassifyWait_EveryProducibleOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		want  WaitOutcome
		label string
	}{
		{"an admission", nil, WaitGranted, "granted"},
		{"the queue deadline", exec.ErrQueueTimeout, WaitExpired, "expired"},
		{"the target was removed", exec.ErrTargetGone, WaitTargetGone, "target_gone"},
		{"the instance is closing", exec.ErrEngineClosing, WaitShuttingDown, "shutting_down"},
		{"every connection is in a transaction", exec.ErrAllCapacityInTransaction,
			WaitAllCapacityInTransaction, "all_capacity_in_transaction"},
		{"the caller stopped waiting", context.Canceled, WaitCancelledByRequest, "cancelled_by_request"},
		{"the caller's deadline passed", context.DeadlineExceeded, WaitCancelledByRequest, "cancelled_by_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassifyWait(tc.err)
			if !ok {
				t.Fatalf("%v did not classify; a producible outcome with no label cannot be "+
					"counted at all", tc.err)
			}
			if got != tc.want {
				t.Fatalf("classified as %d, want %d", got, tc.want)
			}
			l, has := got.Label()
			if !has || l != tc.label {
				t.Errorf("label = %q (present=%v), want %q", l, has, tc.label)
			}
		})
	}
}

// NO FALLBACK BUCKET. The decoy is the whole point: an error the set does not
// know must fail to classify rather than land somewhere plausible.
func TestClassifyWait_AnUnknownErrorGetsNoLabel(t *testing.T) {
	got, ok := ClassifyWait(errors.New("something nobody mapped"))
	if ok {
		l, _ := got.Label()
		t.Fatalf("an unmapped error classified as %d (%q); a trailing catch-all arm would "+
			"absorb every sentinel added later and the breakdown would stop describing "+
			"anything while the total stayed plausible", got, l)
	}
	if _, has := OutcomeUnknown.Label(); has {
		t.Error("the zero value has a label, so an unclassified outcome can be reported as a real one")
	}
}

// EVERY DECLARED OUTCOME HAS A DISTINCT, NON-EMPTY LABEL.
func TestWaitOutcome_LabelsAreDistinctAndTotal(t *testing.T) {
	declared := []WaitOutcome{
		WaitGranted, WaitExpired, WaitTargetGone, WaitShuttingDown,
		WaitCancelledByRequest, WaitAllCapacityInTransaction,
	}
	seen := map[string]WaitOutcome{}
	for _, o := range declared {
		l, ok := o.Label()
		if !ok || l == "" {
			t.Errorf("outcome %d has no label", o)
			continue
		}
		if prev, dup := seen[l]; dup {
			t.Errorf("outcomes %d and %d share the label %q; two outcomes under one series "+
				"cannot be told apart by the person reading it", prev, o, l)
		}
		seen[l] = o
	}
	if len(seen) != len(label) {
		t.Errorf("the map carries %d labels but %d outcomes are declared here; one of them "+
			"has drifted from the other", len(label), len(seen))
	}
}

// THE DRIFT GUARD, AND THE REASON THIS FILE PARSES core/exec.
//
// The failure this is written against is not a wrong label. It is somebody
// adding a new Err… sentinel to the queue, wiring it into a resolve path, and
// never coming here — after which the outcome is real, reachable, and
// uncounted, while every cell above still passes.
//
// So it reads core/exec/acquire_queue.go's own declarations and requires each
// to be either CLASSIFIED or named in notWaitOutcomes below with a reason. A
// new sentinel is in neither, and this fails.
func TestWaitOutcome_EverySchedulerSentinelIsAccountedFor(t *testing.T) {
	// Sentinels declared in the queue that are NOT how a wait ends. Each needs
	// a reason, so the list cannot become a place to park inconvenient ones.
	notWaitOutcomes := map[string]string{}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../exec/acquire_queue.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the queue: %v", err)
	}

	var sentinels []string
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Err") {
					sentinels = append(sentinels, name.Name)
				}
			}
		}
		return true
	})
	if len(sentinels) == 0 {
		t.Fatal("found no Err… sentinels in core/exec/acquire_queue.go; the walk has stopped " +
			"observing anything and would pass no matter what was added there")
	}

	byName := map[string]error{
		"ErrQueueTimeout":             exec.ErrQueueTimeout,
		"ErrAllCapacityInTransaction": exec.ErrAllCapacityInTransaction,
		"ErrTargetGone":               exec.ErrTargetGone,
		"ErrEngineClosing":            exec.ErrEngineClosing,
	}
	for _, name := range sentinels {
		if reason, excused := notWaitOutcomes[name]; excused {
			if reason == "" {
				t.Errorf("%s is excused with no reason", name)
			}
			continue
		}
		sentinel, known := byName[name]
		if !known {
			t.Errorf("core/exec declares %s and this package has never heard of it. Either "+
				"classify it in ClassifyWait, or list it in notWaitOutcomes with the reason "+
				"it is not how a wait ends. A reachable outcome that nothing counts is the "+
				"drift this guard exists to stop", name)
			continue
		}
		if _, ok := ClassifyWait(sentinel); !ok {
			t.Errorf("%s is declared by core/exec and does not classify", name)
		}
	}
}
