package exec

import (
	"sync"
	"testing"
	"time"
)

// (was: the cells pairing a policy reload's timeouts with the instance
// budget's generation. A reload no longer moves a budget: each connection
// pool's ledger keeps its own generation, published by UpdatePool.)

// TWO SETTERS AT ONCE GET TWO DISTINCT, ASCENDING GENERATIONS.
//
// SetBudget used to read generation+1 under the lock, release it, and reacquire
// to publish. Two callers could read the same current value and publish the
// same next one, or publish in the reverse order to the one they chose in. A
// generation that repeats or goes backwards is worse than none: the whole point
// of the number is to let two observers say whether they saw one publication or
// two.
//
// THE BARRIER SITS BETWEEN SELECTION AND PUBLICATION, and that placement is the
// cell's entire discriminating power. The rejected shape puts that seam OUTSIDE
// the lock; the correct one keeps it inside. So pausing there blocks the second
// caller in the correct shape and lets it through in the rejected one.
//
// A barrier inside the publication lock -- which is where this cell first put
// it -- cannot tell them apart: the second caller blocks in both shapes, reads
// a fresh value afterwards, and the rejected shape passes.
func TestPolicyCoherence_ConcurrentSettersCannotReuseAGeneration(t *testing.T) {
	t.Parallel()
	led := newPermitLedger(25)

	atBarrier := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	led.testAfterSelect = func() {
		once.Do(func() {
			close(atBarrier)
			<-release
		})
	}

	first := make(chan error, 1)
	go func() { first <- led.SetBudget(40) }()

	select {
	case <-atBarrier:
	case <-time.After(5 * time.Second):
		t.Fatal("the first setter never reached the barrier")
	}

	// The first setter has CHOSEN and not published. In the correct shape it
	// still holds the lock, so a second setter cannot choose from the stale
	// value it left behind.
	secondChose := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		close(secondChose)
		second <- led.SetBudget(41)
	}()
	<-secondChose

	select {
	case <-second:
		t.Fatal("a second setter completed while the first held its selection; both can " +
			"then choose the same number")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first setter: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second setter: %v", err)
	}

	// TWO PUBLICATIONS, TWO GENERATIONS, ascending -- and the later one wins
	// the budget, because the lock serialises them and the last to hold it is
	// the state that stands.
	got := led.Snapshot()
	if got.Generation != 2 {
		t.Errorf("generation after two setters = %d, want 2 — one of them reused or "+
			"reversed the other's number", got.Generation)
	}
	if got.Configured != 41 {
		t.Errorf("budget = %d, want 41 from the setter that held the lock last", got.Configured)
	}
}
