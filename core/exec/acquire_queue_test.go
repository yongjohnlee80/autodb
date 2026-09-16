package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// newQueuedLedger is a budget-N ledger with a live queue.
//
// BUDGET IS THE OPERATOR'S TOTAL and one slot is the control lane's, so the
// ordinary allowance is N-1. Every cell below states its ordinary allowance
// rather than the budget, because the allowance is what the waiters compete
// for and a cell written against the budget is off by one in the direction
// that hides starvation.
func newQueuedLedger(t *testing.T, budget int) *permitLedger {
	t.Helper()
	l := newPermitLedger(budget)
	l.queue = newAcquireQueue()
	return l
}

// THE OLDEST WAITER IS SERVED FIRST, ACROSS TARGETS AND NOT WITHIN THEM.
//
// This is the property a per-target queue cannot give. Three requests arrive
// for three different connections against a single shared slot; the order they
// are served in must be the order they asked, not the order their targets
// happen to appear in a map. A map-ordered implementation passes a
// single-target test and fails this one.
func TestAcquireQueue_ServesInArrivalOrderAcrossTargets(t *testing.T) {
	l := newQueuedLedger(t, 2) // ordinary allowance 1
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatalf("taking the only ordinary slot: %v", err)
	}

	const waiters = 3
	served := make(chan int64, waiters)
	var enqueued sync.WaitGroup
	for i := range waiters {
		connID := int64(i + 10)
		enqueued.Add(1)
		go func() {
			// Registering in order is the point, so each goroutine announces
			// its arrival only once it is actually in line.
			for l.queue.DepthFor(connID) == 0 {
				if l.queue.Depth() == int(connID-10) {
					enqueued.Done()
					break
				}
				time.Sleep(time.Millisecond)
			}
		}()
		go func() {
			p, werr := l.AcquireOrWait(context.Background(), connID)
			if werr != nil {
				served <- -1
				return
			}
			served <- connID
			p.Release()
		}()
		// Arrival order must be deterministic for this assertion to mean
		// anything, so each waiter is confirmed in line before the next asks.
		deadline := time.Now().Add(5 * time.Second)
		for l.queue.Depth() < i+1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	if got := l.queue.Depth(); got != waiters {
		t.Fatalf("queue depth = %d, want %d", got, waiters)
	}

	held.Release() // releases cascade: each served waiter frees the slot again

	var order []int64
	for range waiters {
		select {
		case id := <-served:
			order = append(order, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d waiters were served: %v", len(order), waiters, order)
		}
	}
	for i, id := range order {
		if want := int64(i + 10); id != want {
			t.Errorf("position %d served connection %d, want %d — the queue is not "+
				"arrival-ordered across targets, so a busy target can starve a quiet one",
				i, id, want)
		}
	}
}

// A CALLER THAT GIVES UP LEAVES THE LINE, AND LEAVES NOTHING BEHIND.
//
// An abandoned waiter that stays in the queue is worse than a leak: the next
// release is handed to somebody who is not listening, and the slot is lost
// until restart while the queue still reports depth.
func TestAcquireQueue_AbandonedWaiterLeavesNoSlotBehind(t *testing.T) {
	l := newQueuedLedger(t, 2)
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, werr := l.AcquireOrWait(ctx, 2); done <- werr }()
	for l.queue.Depth() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if werr := <-done; !errors.Is(werr, context.Canceled) {
		t.Errorf("an abandoned wait returned %v, want the caller's cancellation", werr)
	}
	if d := l.queue.Depth(); d != 0 {
		t.Fatalf("queue depth = %d after the caller gave up, want 0", d)
	}

	held.Release()
	// The slot must be takeable again: if the abandoned waiter had been handed
	// it, this acquire would block until the wait expired.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	p, err := l.AcquireOrWait(ctx2, 3)
	if err != nil {
		t.Fatalf("the slot was not recoverable after an abandoned wait: %v", err)
	}
	p.Release()
}

// EVERY SLOT HELD BY A LIVE TRANSACTION IS REFUSED BEFORE THE QUEUE, NOT AFTER
// NINETY SECONDS OF IT.
//
// Nothing is reclaimable in that state, so queueing would mean waiting the
// whole server wait to be told what was already known. The refusal is also
// pre-enqueue ONLY: the cell asserts the request never entered the queue,
// because an occurrence claiming "we never queued you" must be true.
func TestAcquireOrWait_AllCapacityInTransactionRefusesBeforeQueueing(t *testing.T) {
	l := newQueuedLedger(t, 2) // ordinary allowance 1
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	l.inTransaction = func() int { return 1 } // the one outstanding slot is in a transaction

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = l.AcquireOrWait(ctx, 2)
	if !errors.Is(err, ErrAllCapacityInTransaction) {
		t.Fatalf("err = %v, want the all-in-transaction refusal", err)
	}
	if d := l.queue.Depth(); d != 0 {
		t.Errorf("the refused request entered the queue (depth %d); the identity claims "+
			"it never did", d)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the refusal took %s; it is meant to be immediate rather than a "+
			"ninety-second wait for the same answer", waited)
	}
}

// AND A SLOT NOT HELD BY A TRANSACTION QUEUES INSTEAD OF BEING REFUSED.
//
// The negative direction of the cell above. Without it, a bug that refused
// everything would pass the refusal test and starve every waiter.
func TestAcquireOrWait_ReclaimableCapacityQueuesRatherThanRefusing(t *testing.T) {
	l := newQueuedLedger(t, 2)
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	l.inTransaction = func() int { return 0 } // outstanding, but not in a transaction

	got := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, werr := l.AcquireOrWait(ctx, 2)
		if werr == nil {
			p.Release()
		}
		got <- werr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for l.queue.Depth() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if l.queue.Depth() != 1 {
		t.Fatal("a reclaimable-capacity request was not queued")
	}
	held.Release()
	if werr := <-got; werr != nil {
		t.Fatalf("the queued request was not granted after the release: %v", werr)
	}
}

// THE WAIT IS THE EARLIER OF THE SERVER'S NINETY SECONDS AND THE CALLER'S OWN
// DEADLINE.
//
// Driven through the caller's deadline rather than by waiting ninety seconds,
// because a cell that sleeps for the server bound is a cell nobody runs.
func TestAcquireQueue_CallerDeadlineCapsTheServerWait(t *testing.T) {
	l := newQueuedLedger(t, 2)
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	l.inTransaction = func() int { return 0 }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = l.AcquireOrWait(ctx, 2)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the wait returned a permit although no slot was released")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("the wait took %s; the caller's deadline did not cap the server wait", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrQueueTimeout) {
		t.Errorf("err = %v, want the caller's deadline or the queue timeout", err)
	}
	if d := l.queue.Depth(); d != 0 {
		t.Errorf("queue depth = %d after the wait ended, want 0", d)
	}
}

// THE SERVER WAIT IS NINETY SECONDS, STATED ONCE.
//
// Pinned so a refactor that recomputes it from a transaction bound -- which is
// exactly what the earlier design did, when that bound was also 90s and later
// became two hours -- fails here rather than silently changing how long a
// person waits for an answer.
func TestAcquireQueue_TheServerWaitIsNinetySecondsAndNotDerived(t *testing.T) {
	if queueWait != 90*time.Second {
		t.Fatalf("queueWait = %s, want 90s", queueWait)
	}
	if queueWait == defaultTxLimits().idleInTx {
		t.Error("the queue wait equals the idle-in-transaction bound; they were coupled " +
			"once and decoupling them is the point — two hours is a transaction " +
			"tolerance, not a tolerable wait for an answer")
	}
}
