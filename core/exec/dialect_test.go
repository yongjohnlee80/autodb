package exec

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// THE NEGATIVE CELL, which is the one that matters for a capability.
//
// A capability probe is trivially satisfiable in the direction everyone tests:
// postgres has a belt, the belt gets armed, green. The direction that decides
// whether the interface is worth having is the ABSENT one — a target without
// the capability must take the no-op branch and the caller must carry on. An
// interface tested only where it is present is an interface nobody has proven
// is optional.
//
// Written per engine so a third engine arriving without a belt is a decision
// someone makes here rather than a default nobody notices.
func TestEveryEngineWithoutABeltTakesTheAbsentBranch(t *testing.T) {
	for _, n := range engine.All() {
		_, has := dialectFor(n).(StatementTimeoutBelt)
		if has != n.HasServerStatementTimeout() {
			t.Errorf("engine %s: the dialect %s a StatementTimeoutBelt but the "+
				"capability table says %v. The interface and the predicate "+
				"describe the same fact and must not drift — the predicate is "+
				"what a reader consults, the interface is what runs.",
				n, map[bool]string{true: "implements", false: "does not implement"}[has],
				n.HasServerStatementTimeout())
		}
		if has {
			continue
		}
		// AND THE ABSENT BRANCH MUST BE REACHED WITHOUT A CONNECTION. A nil tx
		// is the strongest available witness that nothing was executed: if the
		// probe ever stopped short-circuiting, this panics rather than quietly
		// passing on a recording fake that nobody inspected.
		if err := armServerBelt(context.Background(), nil, n, defaultTxLimits()); err != nil {
			t.Errorf("engine %s has no belt, so arming one must be a no-op, and "+
				"it returned %v", n, err)
		}
	}
}

// The present branch must actually execute, on the transaction it is given.
//
// The companion to the cell above: together they say the probe DISCRIMINATES,
// which neither says alone. Without this one, a dialectFor that returned nil
// for everything would pass the absent cell for every engine.
func TestTheBeltIsArmedOnTheTransactionItIsGiven(t *testing.T) {
	armed := 0
	for _, n := range engine.All() {
		belt, ok := dialectFor(n).(StatementTimeoutBelt)
		if !ok {
			continue
		}
		rec := &recordingTx{}
		if err := belt.ArmIdleTransactionBelt(context.Background(), rec, 90); err != nil {
			t.Fatalf("engine %s: arming the belt: %v", n, err)
		}
		if len(rec.execs) != 1 {
			t.Fatalf("engine %s: the belt ran %d statement(s), want exactly one",
				n, len(rec.execs))
		}
		armed++
	}
	if armed == 0 {
		t.Fatal("no engine implements StatementTimeoutBelt, so the cell above " +
			"holds for every engine vacuously — which is what a dialectFor " +
			"returning nil for everything would look like")
	}
}
