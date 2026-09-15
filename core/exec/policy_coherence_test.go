package exec

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A READOUT MUST REPORT A MOMENT THAT EXISTED.
//
// Settings pairs the timeouts with the connection ledger. Those are published
// together by a reload, and the publication holds the ledger's lock while it
// swaps the policy pointer -- so a WRITER cannot interleave them. That was not
// enough: a READER that loaded the policy pointer first, was descheduled while
// a reload published, and then took the ledger snapshot would pair the OLD
// timeouts with the NEW budget.
//
// That tuple never existed at any instant, and the person holding it is the
// operator who just made the change and is watching for it to land.
//
// The barrier below makes the interleaving DETERMINISTIC rather than raced
// for: the reload takes the ledger lock and stops there, before publishing
// anything. A reader that loads the policy outside that lock will do so now,
// against the old value, and then block for the new ledger.
func TestPolicyCoherence_AReadoutIsNeverAMixedTuple(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	before := f.eng.Settings()
	if before.PolicyGeneration != before.TargetConns.Generation {
		t.Fatalf("policy generation %d and ledger generation %d differ before any reload; "+
			"two counters that start apart can never agree afterwards",
			before.PolicyGeneration, before.TargetConns.Generation)
	}

	atBarrier := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.eng.targetPermits.testBeforePublish = func() {
		once.Do(func() {
			close(atBarrier)
			<-release
		})
	}

	spec := goodSpec()
	spec.IdleInTxTimeout = 45 * time.Minute
	spec.MaxTargetConns = 33

	reloaded := make(chan error, 1)
	go func() {
		_, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
		reloaded <- err
	}()

	select {
	case <-atBarrier:
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never reached the publication barrier")
	}

	// The reload now holds the ledger lock and has published NOTHING. A reader
	// that takes both halves under that lock cannot proceed; one that loads
	// the policy first would sample the old value here and pair it with the
	// ledger it reads after the release.
	got := make(chan Settings, 1)
	go func() { got <- f.eng.Settings() }()

	select {
	case s := <-got:
		t.Fatalf("Settings returned while a publication held the ledger lock, so it read "+
			"the policy outside that lock: idle=%v budget=%d. That is the ordering that "+
			"produces a tuple no moment ever held",
			s.IdleInTxTimeout, s.TargetConns.Configured)
	case <-time.After(250 * time.Millisecond):
		// Blocked, which is the correct behaviour.
	}

	close(release)
	if err := <-reloaded; err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}

	select {
	case s := <-got:
		// WHOLLY THE NEW TUPLE. Not the old timeouts beside the new budget,
		// and not the reverse.
		if s.IdleInTxTimeout != 45*time.Minute || s.TargetConns.Configured != 33 {
			t.Errorf("mixed tuple: idle=%v budget=%d, want 45m0s and 33",
				s.IdleInTxTimeout, s.TargetConns.Configured)
		}
		if s.PolicyGeneration != s.TargetConns.Generation {
			t.Errorf("policy generation %d, ledger generation %d — a readout that reports "+
				"them together must report one fact, not two counters",
				s.PolicyGeneration, s.TargetConns.Generation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Settings never returned after the barrier released")
	}
}

// THE TWO GENERATIONS ARE ONE NUMBER, at every point in a policy's life.
//
// They used to start apart -- the configured policy at 0 and the ledger at 1 --
// and each reload incremented both independently, so after the first reload
// the readout showed 1 beside 2 and called it "one generation". A permanent
// offset is not a coincidence a reader can be asked to allow for.
func TestPolicyCoherence_TheGenerationsAreOneNumber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	check := func(when string, s Settings, want uint64) {
		t.Helper()
		if s.PolicyGeneration != s.TargetConns.Generation {
			t.Errorf("%s: policy %d, ledger %d", when, s.PolicyGeneration, s.TargetConns.Generation)
		}
		if s.PolicyGeneration != want {
			t.Errorf("%s: generation = %d, want %d", when, s.PolicyGeneration, want)
		}
	}

	check("at construction", f.eng.Settings(), 0)

	for i := uint64(1); i <= 3; i++ {
		spec := goodSpec()
		spec.MaxTargetConns = 20 + int(i)
		got, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		check("after reload", got, i)
		check("read back", f.eng.Settings(), i)
	}

	// AND ACROSS A RESTART. The stored generation is restored to both sides,
	// so a restarted daemon does not silently reset one of them.
	restarted := New(f.store, f.svc, WithMaxRows(3), WithTargetConnBudget(25))
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.LoadDurablePolicy(ctx); err != nil {
		t.Fatalf("LoadDurablePolicy: %v", err)
	}
	check("after restart", restarted.Settings(), 3)
}

// AND THEY STAY EQUAL WHILE A LOWERED BUDGET DRAINS, which is the state an
// operator is most likely to be reading.
func TestPolicyCoherence_TheGenerationsHoldDuringADrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	// Take permits so lowering the budget leaves the ledger draining.
	var held []*Permit
	for i := 0; i < 10; i++ {
		p, err := f.eng.targetPermits.Acquire()
		if err != nil {
			t.Fatalf("acquiring permit %d: %v", i, err)
		}
		held = append(held, p)
	}
	t.Cleanup(func() {
		for _, p := range held {
			p.Release()
		}
	})

	spec := goodSpec()
	spec.MaxTargetConns = 5
	got, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
	if err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}
	if !got.TargetConns.Draining {
		t.Fatalf("the ledger is not draining, so this cell is not testing a drain: %+v",
			got.TargetConns)
	}
	if got.PolicyGeneration != got.TargetConns.Generation {
		t.Errorf("during a drain: policy %d, ledger %d",
			got.PolicyGeneration, got.TargetConns.Generation)
	}
	// Outstanding may exceed the configured number while it drains; that is
	// correct and must not be rendered as an error.
	if got.TargetConns.Outstanding <= got.TargetConns.Configured {
		t.Errorf("outstanding %d does not exceed configured %d, so the drain is not the "+
			"state this cell claims to read", got.TargetConns.Outstanding, got.TargetConns.Configured)
	}
}

// TWO SETTERS AT ONCE GET TWO DISTINCT, ASCENDING GENERATIONS.
//
// SetBudget used to read generation+1 under the lock, release it, and reacquire
// to publish. Two callers could read the same current value and publish the
// same next one, or publish in the reverse order to the one they chose in. A
// generation that repeats or goes backwards is worse than none: the whole point
// of the number is to let two observers say whether they saw one publication or
// two.
//
// The barrier makes the interleaving deterministic. The first setter is held
// inside the lock; the second is started and must block, because choosing and
// publishing are now one critical section.
func TestPolicyCoherence_ConcurrentSettersCannotReuseAGeneration(t *testing.T) {
	t.Parallel()
	f := policyFixture(t)
	led := f.eng.targetPermits

	atBarrier := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	led.testBeforePublish = func() {
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

	// The first setter holds the lock and has chosen but not published. A
	// second setter must not be able to choose from the same stale value.
	second := make(chan error, 1)
	go func() { second <- led.SetBudget(41) }()

	select {
	case <-second:
		t.Fatal("a second setter chose a generation while the first held the lock; both " +
			"can then publish the same number")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first setter: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second setter: %v", err)
	}

	// TWO PUBLICATIONS, TWO GENERATIONS, and the later one wins the budget --
	// the lock serialises them, so the last to hold it is the state that
	// stands.
	got := led.Snapshot()
	if got.Generation != 2 {
		t.Errorf("generation after two setters = %d, want 2 — one of them reused the "+
			"other's number", got.Generation)
	}
	if got.Configured != 41 {
		t.Errorf("budget = %d, want 41 from the setter that held the lock last", got.Configured)
	}
}
