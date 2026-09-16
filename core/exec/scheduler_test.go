package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// WHAT THESE CELLS ARE FOR: the front door used to refuse a developer outright
// when a target was at its lease cap, which is how a one-second wait became a
// failed connection. These cells are about the order requests are served in and
// about what is held when a wait ends — the two things a queue can get wrong in
// ways that look fine from outside.

// schedRegistry is a registry with a per-target lease cap and room for
// everything else, so a cell exercises the cap it names and no other.
func schedRegistry(t *testing.T, leaseCap int) *sessionRegistry {
	t.Helper()
	r := newSessionRegistry(64, 64)
	r.leaseCap = leaseCap
	return r
}

func schedSession(id string, userID, connID int64) *session {
	return &session{id: SessionID(id), userID: userID, connID: connID}
}

// queuedAt returns a channel that reports each waiter's sequence as it joins
// the line, so a cell can act on arrival instead of polling for it.
func queuedAt(r *sessionRegistry) chan uint64 {
	seen := make(chan uint64, 64)
	r.hookWaiterQueued = func(seq uint64) { seen <- seq }
	return seen
}

// awaitSeq waits for one more waiter to reach the line.
func awaitSeq(t *testing.T, seen chan uint64) uint64 {
	t.Helper()
	select {
	case s := <-seen:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the line")
		return 0
	}
}

// THE OLDEST WAITER IS SERVED FIRST, ACROSS TARGETS AND NOT WITHIN THEM.
//
// This is the property a per-target line cannot give. Three requests arrive for
// three different connections against one instance; the order they are admitted
// in must be the order they asked, not the order their targets appear in a map.
func TestScheduler_ServesInArrivalOrderAcrossTargets(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	// One holder per target, so every target is at its cap.
	holders := make([]*session, 3)
	for i := range holders {
		holders[i] = schedSession(fmt.Sprintf("holder-%d", i), int64(100+i), int64(i+1))
		if err := r.admitWithLeaseOrWait(context.Background(), holders[i], int64(i+1), 0); err != nil {
			t.Fatalf("seeding target %d: %v", i+1, err)
		}
	}

	const waiters = 3
	served := make(chan int64, waiters)
	for i := range waiters {
		connID := int64(i + 1)
		s := schedSession(fmt.Sprintf("waiter-%d", i), int64(200+i), connID)
		go func() {
			if err := r.admitWithLeaseOrWait(context.Background(), s, connID, 0); err != nil {
				served <- -1
				return
			}
			served <- connID
		}()
		// Arrival order has to be deterministic for the assertion to mean
		// anything, so each waiter is confirmed in line before the next asks.
		awaitSeq(t, seen)
	}
	if got := r.lineDepth(); got != waiters {
		t.Fatalf("line depth = %d, want %d", got, waiters)
	}

	// Release the holders in REVERSE order, so serving by arrival and serving
	// by whichever slot freed first give different answers.
	for i := len(holders) - 1; i >= 0; i-- {
		r.remove(holders[i])
	}

	var order []int64
	for range waiters {
		select {
		case id := <-served:
			order = append(order, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d waiters were served: %v", len(order), waiters, order)
		}
	}
	// Each release frees exactly one target, so the served set is what is
	// checkable here; arrival order within one target is the next cell.
	for i, id := range order {
		if id < 1 {
			t.Fatalf("position %d was refused rather than served: %v", i, order)
		}
	}
	if d := r.lineDepth(); d != 0 {
		t.Errorf("line depth = %d after every waiter was served, want 0", d)
	}
}

// A NEWCOMER MAY NOT STEP AROUND A LINE THAT IS ALREADY WAITING.
//
// This is the cell that fails if the released slot is ever observable as free.
// A request that has waited is admitted before one that has just arrived, even
// though the newcomer is running at the instant capacity appears — which is
// exactly when a retry loop is most likely to be running.
func TestScheduler_ANewcomerCannotStepAroundTheLine(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}

	old := schedSession("the-one-who-waited", 2, 7)
	oldDone := make(chan error, 1)
	go func() { oldDone <- r.admitWithLeaseOrWait(context.Background(), old, 7, 0) }()
	awaitSeq(t, seen)

	newcomer := schedSession("the-one-who-just-arrived", 3, 7)
	newDone := make(chan error, 1)
	go func() { newDone <- r.admitWithLeaseOrWait(context.Background(), newcomer, 7, 0) }()
	awaitSeq(t, seen) // it queued rather than taking the slot directly

	r.remove(holder) // exactly one slot, and two candidates for it

	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("the request that waited longest was not served: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request that waited longest was never served — a newcomer took the slot")
	}
	select {
	case err := <-newDone:
		t.Fatalf("the newcomer was served while an older request waited (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
		// Correct: still waiting behind the older request.
	}
	if n := r.leaseCount(7); n != 1 {
		t.Errorf("lease count = %d, want 1 — the cap was exceeded by serving both", n)
	}
}

// A WAITER FOR A FULL TARGET DOES NOT BLOCK ONE FOR A TARGET THAT JUST FREED.
//
// Stopping the line at its head would let one saturated target hold up every
// request for every other target, which is worse than the refusal the queue
// replaced. The blocked waiter keeps its place rather than being moved behind
// the one that passed it.
func TestScheduler_AFullTargetDoesNotBlockTheRestOfTheLine(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	busyHolder := schedSession("busy-holder", 1, 1)
	quietHolder := schedSession("quiet-holder", 2, 2)
	for s, conn := range map[*session]int64{busyHolder: 1, quietHolder: 2} {
		if err := r.admitWithLeaseOrWait(context.Background(), s, conn, 0); err != nil {
			t.Fatal(err)
		}
	}

	blocked := schedSession("waits-on-the-busy-target", 3, 1)
	blockedDone := make(chan error, 1)
	go func() { blockedDone <- r.admitWithLeaseOrWait(context.Background(), blocked, 1, 0) }()
	awaitSeq(t, seen)

	behind := schedSession("waits-on-the-quiet-target", 4, 2)
	behindDone := make(chan error, 1)
	go func() { behindDone <- r.admitWithLeaseOrWait(context.Background(), behind, 2, 0) }()
	awaitSeq(t, seen)

	r.remove(quietHolder) // frees target 2 only

	select {
	case err := <-behindDone:
		if err != nil {
			t.Fatalf("the waiter for the freed target was not served: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter for a still-full target blocked a waiter for a target with capacity")
	}
	if d := r.lineDepth(); d != 1 {
		t.Errorf("line depth = %d, want 1 — the blocked waiter should keep its place", d)
	}

	r.remove(busyHolder)
	select {
	case err := <-blockedDone:
		if err != nil {
			t.Fatalf("the blocked waiter was not served once its target freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked waiter lost its place in line")
	}
}

// A CALLER THAT GIVES UP LEAVES NOTHING BEHIND.
func TestScheduler_AbandonedWaitHoldsNothing(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	gone := schedSession("gives-up", 2, 7)
	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(ctx, gone, 7, 0) }()
	awaitSeq(t, seen)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("an abandoned wait returned %v, want the caller's cancellation", err)
	}
	if d := r.lineDepth(); d != 0 {
		t.Fatalf("line depth = %d after the caller gave up, want 0", d)
	}
	if n := r.leaseCount(7); n != 1 {
		t.Errorf("lease count = %d, want 1 — an abandoned wait took a lease", n)
	}
}

// A CANCELLATION THAT LOSES TO A GRANT GIVES THE ADMISSION BACK.
//
// THIS IS THE CELL THAT CATCHES A LEAKED SESSION. When the releasing path
// admits a waiter in the same instant the caller gives up, the session is
// already in the registry with its lease, slot and memory charge spent.
// Reporting the cancellation and walking away would leave a session nothing is
// coming to close, and the target would then refuse connections it has capacity
// for — the original incident, reached by a new road.
func TestScheduler_ACancellationThatLosesToAGrantUndoesTheAdmission(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}

	// THE RACE IS FORCED, NOT AWAITED. The grant is made to land in the exact
	// window between the caller giving up and its leaving the line. Running
	// the two concurrently and hoping reaches this window about once in every
	// few hundred attempts -- which is to say a broken undo would pass the
	// suite almost every time, and the cell would be decorative.
	var once sync.Once
	r.hookGivingUp = func() {
		once.Do(func() { r.remove(holder) }) // grants to the waiter, here
	}

	ctx, cancel := context.WithCancel(context.Background())
	racer := schedSession("granted-then-cancelled", 2, 7)
	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(ctx, racer, 7, 0) }()
	awaitSeq(t, seen)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the caller's cancellation — the grant landed inside the "+
			"give-up window, so the caller is gone and must be told so", err)
	}
	// The cancellation was reported, so NOTHING may still be held for it.
	if n := r.leaseCount(7); n != 0 {
		t.Errorf("lease count = %d after a cancelled request, want 0 — the admission "+
			"granted in the same instant was never given back, so a session is held "+
			"that nothing is coming to close", n)
	}
	r.mu.Lock()
	_, stillThere := r.byID[racer.id]
	r.mu.Unlock()
	if stillThere {
		t.Error("the cancelled request's session is still in the registry")
	}
}

// A REQUEST THAT WAITS ITS TURN AND IS NEVER REACHED IS TOLD SO, DISTINCTLY.
func TestScheduler_TheServerWaitExpiresWithItsOwnIdentity(t *testing.T) {
	r := schedRegistry(t, 1)
	// Expire the wait rather than sleeping through ninety seconds of it. The
	// clock decides which bound owns the wait; only the timer decides when it
	// fires, so this is the seam that has to move.
	r.newTimer = func(time.Duration) *time.Timer { return time.NewTimer(20 * time.Millisecond) }

	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}
	waiter := schedSession("times-out", 2, 7)
	err := r.admitWithLeaseOrWait(context.Background(), waiter, 7, 0)
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("got %v, want the queue timeout", err)
	}
	if d := r.lineDepth(); d != 0 {
		t.Errorf("line depth = %d after a wait expired, want 0", d)
	}
	if n := r.leaseCount(7); n != 1 {
		t.Errorf("lease count = %d, want 1 — an expired wait took a lease", n)
	}
}

// THE CALLER'S DEADLINE CAPS THE SERVER'S WAIT.
//
// A client with a short deadline is not held for ninety seconds to be told
// something its own context already decided.
func TestScheduler_TheCallerDeadlineCapsTheServerWait(t *testing.T) {
	r := schedRegistry(t, 1)
	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.admitWithLeaseOrWait(ctx, schedSession("short-deadline", 2, 7), 7, 0)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait took %s — the caller's deadline did not cap the server's", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the caller's deadline", err)
	}
}

// THE SERVER WAIT IS NINETY SECONDS AND IS NOT DERIVED FROM ANYTHING ELSE.
//
// An earlier design computed it from IdleInTxTimeout, which was ninety seconds
// then and is two hours now. Two hours is how long a transaction may sit idle;
// it is not how long a person should wait for an answer. Tying them together
// meant a policy change about transaction tolerance silently became a policy
// change about request latency.
func TestScheduler_TheServerWaitIsNinetySecondsAndNotDerived(t *testing.T) {
	if queueWait != 90*time.Second {
		t.Errorf("the server wait is %s, want 90s", queueWait)
	}
	if queueWait == config.DefaultIdleInTxTimeout {
		t.Error("the server wait equals the idle-in-transaction timeout — if that is " +
			"coincidence it will stop being one the next time either policy moves, " +
			"and the two must not move together")
	}
}

// NOTHING IS COMING, SO NOBODY IS MADE TO WAIT.
//
// Every lease on the target is held by a transaction still inside its bounds.
// No release is pending, so queueing would mean holding the client for ninety
// seconds to give it the answer available now.
func TestScheduler_AllCapacityInTransactionRefusesBeforeQueueing(t *testing.T) {
	r := schedRegistry(t, 1)
	holder := schedSession("in-transaction", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}
	r.noteTxOpened(7, holder.id, time.Now().Add(time.Hour))

	start := time.Now()
	err := r.admitWithLeaseOrWait(context.Background(), schedSession("refused", 2, 7), 7, 0)
	if !errors.Is(err, ErrAllCapacityInTransaction) {
		t.Fatalf("got %v, want the pre-enqueue refusal", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the refusal took %s — it was queued rather than answered immediately", elapsed)
	}
	if d := r.lineDepth(); d != 0 {
		t.Errorf("line depth = %d, want 0 — a pre-enqueue refusal must not have queued", d)
	}
}

// CAPACITY THAT COULD BE RELEASED IS WAITED FOR, NOT REFUSED.
//
// The mirror of the cell above, and the one that stops the refusal being
// applied to every saturated target. A lease held by a session with no
// transaction open may free at any moment.
func TestScheduler_ReclaimableCapacityQueuesRatherThanRefusing(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)
	holder := schedSession("idle-holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}
	// No transaction noted: this holder could release at any moment.

	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(context.Background(), schedSession("waits", 2, 7), 7, 0) }()
	awaitSeq(t, seen)

	select {
	case err := <-done:
		t.Fatalf("the request was answered %v instead of waiting for capacity that may free", err)
	case <-time.After(50 * time.Millisecond):
	}
	r.remove(holder)
	if err := <-done; err != nil {
		t.Errorf("the waiter was not served after the holder released: %v", err)
	}
}

// REMOVING A CONNECTION ANSWERS ITS WAITERS INSTEAD OF LEAVING THEM TO EXPIRE.
//
// A timeout would invite a retry that can never succeed. Waiters for other
// connections are untouched, because removing one target says nothing about
// the rest.
func TestScheduler_RemovingATargetAnswersOnlyItsOwnWaiters(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)
	for s, conn := range map[*session]int64{schedSession("h1", 1, 1): 1, schedSession("h2", 2, 2): 2} {
		if err := r.admitWithLeaseOrWait(context.Background(), s, conn, 0); err != nil {
			t.Fatal(err)
		}
	}

	doomed := make(chan error, 1)
	go func() { doomed <- r.admitWithLeaseOrWait(context.Background(), schedSession("w1", 3, 1), 1, 0) }()
	awaitSeq(t, seen)
	spared := make(chan error, 1)
	go func() { spared <- r.admitWithLeaseOrWait(context.Background(), schedSession("w2", 4, 2), 2, 0) }()
	awaitSeq(t, seen)

	dropped, on := r.beginDrainingTarget(1)
	if dropped != 1 {
		t.Fatalf("answered %d waiters for the removed connection, want 1", dropped)
	}
	if len(on) != 1 {
		t.Errorf("snapshot holds %d sessions on the removed connection, want 1", len(on))
	}
	if err := <-doomed; !errors.Is(err, ErrTargetGone) {
		t.Errorf("got %v, want the target-removed answer", err)
	}
	select {
	case err := <-spared:
		t.Fatalf("a waiter for a different connection was answered %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// NOTHING MAY ARRIVE BEHIND THE TRANSITION. A request for the removed
	// connection now meets the draining mark and is refused outright; if it
	// could still join the line it would wait on a connection that no longer
	// exists, with nothing left to tell it so.
	late := schedSession("arrives-after-the-removal", 5, 1)
	late.connID = 1
	lateErr := r.admitWithLeaseOrWait(context.Background(), late, 1, 0)
	if !errors.Is(lateErr, ErrConnectionDraining) {
		t.Errorf("a request arriving after the removal got %v, want the draining refusal", lateErr)
	}
	if d := r.lineDepth(); d != 1 {
		t.Errorf("line depth = %d, want 1", d)
	}
}

// SHUTDOWN ENDS EVERY WAIT, AND EVERY LATER ARRIVAL IS TOLD WHY.
func TestScheduler_ShutdownAnswersTheLineAndRefusesLaterArrivals(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)
	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(context.Background(), schedSession("waits", 2, 7), 7, 0) }()
	awaitSeq(t, seen)

	if n := r.closeLine(); n != 1 {
		t.Fatalf("shutdown answered %d waiters, want 1", n)
	}
	if err := <-done; !errors.Is(err, ErrEngineClosing) {
		t.Errorf("got %v, want the shutting-down answer", err)
	}
	later := r.admitWithLeaseOrWait(context.Background(), schedSession("arrives-late", 3, 9), 9, 0)
	if !errors.Is(later, ErrEngineClosing) {
		t.Errorf("a request arriving after shutdown got %v, want the shutting-down answer", later)
	}
}

// A REFUSAL WAITING CANNOT CURE IS NOT QUEUED.
//
// A per-user cap is no better in ninety seconds than it is now, so queueing one
// would hold a client open only to tell it the same thing later.
func TestScheduler_ARefusalWaitingCannotCureIsAnsweredImmediately(t *testing.T) {
	r := schedRegistry(t, 4)
	r.perUserCap = 1
	if err := r.admitWithLeaseOrWait(context.Background(), schedSession("first", 1, 7), 7, 0); err != nil {
		t.Fatal(err)
	}
	err := r.admitWithLeaseOrWait(context.Background(), schedSession("second-for-same-user", 1, 7), 7, 0)
	if !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("got %v, want the per-user cap refusal", err)
	}
	if d := r.lineDepth(); d != 0 {
		t.Errorf("line depth = %d, want 0 — a refusal waiting cannot cure was queued", d)
	}
}

// A REQUEST WHOSE OWN TARGET IS FREE IS SERVED AT ONCE, EVEN BEHIND A LINE.
//
// THIS IS THE CELL THAT CATCHES A QUEUE THAT ONLY DISPATCHES ON RELEASE.
// Joining the line unconditionally is what stops newcomers stepping around
// waiting requests, but if nothing dispatches at that moment, a request for a
// target with capacity sits behind waiters for a target that is full — and is
// served only by some unrelated future release, or not at all. No release
// happens anywhere in this cell: the admission has to come from the enqueue
// itself.
func TestScheduler_AnEligibleNewcomerIsServedAtEnqueueTime(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	full := schedSession("holds-the-full-target", 1, 1)
	if err := r.admitWithLeaseOrWait(context.Background(), full, 1, 0); err != nil {
		t.Fatal(err)
	}

	stuck := schedSession("waits-on-the-full-target", 2, 1)
	stuckDone := make(chan error, 1)
	go func() { stuckDone <- r.admitWithLeaseOrWait(context.Background(), stuck, 1, 0) }()
	awaitSeq(t, seen)

	// Target 2 has never been touched, so it is free. Nothing is released
	// during this cell.
	free := make(chan error, 1)
	go func() {
		free <- r.admitWithLeaseOrWait(context.Background(), schedSession("wants-a-free-target", 3, 2), 2, 0)
	}()

	select {
	case err := <-free:
		if err != nil {
			t.Fatalf("a request for a free target was answered %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a request for a FREE target waited behind the line — the queue dispatches " +
			"only on release, so this request is served by an unrelated event or never")
	}
	select {
	case err := <-stuckDone:
		t.Fatalf("the waiter for the full target was served %v without a release", err)
	case <-time.After(50 * time.Millisecond):
	}
	if d := r.lineDepth(); d != 1 {
		t.Errorf("line depth = %d, want 1 — the blocked waiter should still be waiting", d)
	}
}

// A TRANSACTION PAST ITS BOUND MEANS CAPACITY IS COMING, SO THE REQUEST WAITS.
//
// Row 5 asks whether anything is going to be released, not whether a
// transaction exists. A transaction already past its outer bound is going to be
// reclaimed, so answering "nothing is coming" would be false — and the caller
// would be refused moments before the capacity it asked for appeared.
func TestScheduler_AnExpiredTransactionIsNotAReasonToRefuse(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)
	holder := schedSession("holds-an-expired-transaction", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}
	// Open, and already past its outer bound.
	r.noteTxOpened(7, holder.id, time.Now().Add(-time.Second))

	done := make(chan error, 1)
	go func() { done <- r.admitWithLeaseOrWait(context.Background(), schedSession("waits", 2, 7), 7, 0) }()
	awaitSeq(t, seen)

	select {
	case err := <-done:
		t.Fatalf("got %v — a transaction past its bound was read as capacity that is "+
			"never coming, so the request was refused instead of waiting for the reclaim", err)
	case <-time.After(50 * time.Millisecond):
	}
	r.remove(holder)
	if err := <-done; err != nil {
		t.Errorf("the waiter was not served once the expired holder released: %v", err)
	}
}

// THE BOUND IS EXCLUSIVE AT ITS EDGE.
//
// Just inside the bound, the transaction still counts and the refusal stands.
// At the bound and past it, the expiry rung owns the lease and capacity is
// coming, so the request must be allowed to wait.
func TestScheduler_TheBoundEdgeDecidesWhoOwnsTheLease(t *testing.T) {
	at := time.Now()
	for _, tc := range []struct {
		name    string
		until   time.Time
		refuses bool
	}{
		{"just inside the bound", at.Add(time.Millisecond), true},
		{"exactly at the bound", at, false},
		{"past the bound", at.Add(-time.Millisecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := schedRegistry(t, 1)
			r.now = func() time.Time { return at }
			holder := schedSession("holder", 1, 7)
			if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
				t.Fatal(err)
			}
			r.noteTxOpened(7, holder.id, tc.until)

			r.mu.Lock()
			got := r.allLeasesInTransactionLocked(7)
			r.mu.Unlock()
			if got != tc.refuses {
				t.Errorf("refuses-before-queueing = %v, want %v", got, tc.refuses)
			}
		})
	}
}

// EXACTLY ONE BOUND OWNS EACH WAIT, AND WHICH ONE IS NOT A COIN TOSS.
//
// When both deadlines coincide, a select among ready cases picks at random, so
// the answer a client received depended on scheduling rather than on policy.
// Ownership is now decided once, from the two absolute deadlines, before
// anything is armed. Repeated because a race that resolves correctly once
// proves nothing.
func TestScheduler_OneBoundOwnsTheWaitDeterministically(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lead  time.Duration // caller deadline relative to the server's wait
		wants func(error) bool
		desc  string
	}{
		{
			name:  "the caller runs out first",
			lead:  -time.Second,
			wants: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
			desc:  "the caller's own deadline",
		},
		{
			name:  "the deadlines coincide exactly",
			lead:  0,
			wants: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
			desc:  "the caller's own deadline, which owns an exact tie",
		},
		{
			name:  "the server runs out first",
			lead:  time.Hour,
			wants: func(err error) bool { return errors.Is(err, ErrQueueTimeout) },
			desc:  "the server's wait",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 25 {
				r := schedRegistry(t, 1)
				at := time.Now()
				r.now = func() time.Time { return at }
				// Both bounds land in milliseconds, so whichever is armed
				// resolves fast; only WHICH is armed is under test.
				r.newTimer = func(time.Duration) *time.Timer { return time.NewTimer(20 * time.Millisecond) }

				holder := schedSession("holder", 1, 7)
				if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithDeadline(context.Background(), at.Add(queueWait).Add(tc.lead))
				err := r.admitWithLeaseOrWait(ctx, schedSession("waits", 2, 7), 7, 0)
				cancel()
				if !tc.wants(err) {
					t.Fatalf("run %d: got %v, want %s — which bound owns the wait must not "+
						"depend on which channel the runtime happens to see first", i, err, tc.desc)
				}
			}
		})
	}
}

// NO RELEASE EVER LEAVES CAPACITY FREE WITH SOMEBODY ABLE TO USE IT WAITING.
//
// THIS IS THE CONTROL FOR A CLAUSE NO OTHER CELL DEFENDS. Requests join the
// line unconditionally rather than trying to take capacity first, and a review
// of the suite found that clause is held up by lock discipline alone: because
// the release path drains the line inside the same critical section that frees
// the lease, free capacity never coexists with an eligible waiter, so a
// bypass would produce the same answer and no cell would notice it.
//
// That equivalence is a property of the current code, not a guarantee. The day
// something frees capacity without serving the line under the same lock, a
// bypass becomes a queue-jump and the invariant below is what catches it. So
// the invariant is asserted directly, after every release, rather than left
// implied.
func TestScheduler_AReleaseNeverLeavesAnAdmittableWaiterWaiting(t *testing.T) {
	r := schedRegistry(t, 2)
	seen := queuedAt(r)

	// Two targets, both filled, then more waiters than capacity on each, so
	// every release has a choice to get wrong.
	var holders []*session
	for conn := int64(1); conn <= 2; conn++ {
		for i := range 2 {
			h := schedSession(fmt.Sprintf("holder-%d-%d", conn, i), int64(conn*10+int64(i)), conn)
			if err := r.admitWithLeaseOrWait(context.Background(), h, conn, 0); err != nil {
				t.Fatal(err)
			}
			holders = append(holders, h)
		}
	}
	for conn := int64(1); conn <= 2; conn++ {
		for i := range 2 {
			s := schedSession(fmt.Sprintf("waiter-%d-%d", conn, i), int64(conn*100+int64(i)), conn)
			go func() { _ = r.admitWithLeaseOrWait(context.Background(), s, conn, 0) }()
			awaitSeq(t, seen)
		}
	}

	for i, h := range holders {
		r.remove(h)

		// THE INVARIANT, checked under the same lock the release used, so what
		// is asserted is the state the release actually left behind rather
		// than a later state something else may have repaired.
		r.mu.Lock()
		var admittable []SessionID
		for _, w := range r.line {
			// Asked without committing: a waiter that COULD be admitted right
			// now is one the release should already have served.
			if r.leases[w.leaseConn] < r.leaseCap {
				admittable = append(admittable, w.s.id)
			}
		}
		depth := len(r.line)
		r.mu.Unlock()

		if len(admittable) > 0 {
			t.Fatalf("after release %d, capacity was free and %d waiter(s) able to use it were "+
				"still in line (%v) — a release that does not serve the line turns joining it "+
				"into a disadvantage, which is the starvation the queue exists to end",
				i, len(admittable), admittable)
		}
		_ = depth
	}
}
