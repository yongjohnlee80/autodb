package exec

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// WHAT THESE CELLS ARE FOR: reclaim takes a physical connection away from a
// session that is still open, so the whole question is whether the holder can
// tell. Every cell below is one way a holder WOULD be able to tell, and asserts
// that the backend stays where it is. The single positive cell is the state in
// which nothing of the holder's exists on that connection.

// reclaimHolder is a session holding a backend with nothing on it.
func reclaimHolder(t *testing.T, connID int64) (*session, *gateConn) {
	t.Helper()
	c := newGateConn()
	return &session{id: SessionID("holder"), userID: 3, connID: connID, pc: c}, c
}

// registryHolding puts sessions in a registry the engine can search.
func registryHolding(sessions ...*session) *sessionRegistry {
	r := newSessionRegistry(10, 10)
	for i, s := range sessions {
		if s.id == "" {
			s.id = SessionID(string(rune('a' + i)))
		}
		r.byID[s.id] = s
	}
	return r
}

// AN IDLE, EMPTY HOLDER GIVES UP ITS BACKEND, AND THE GATE PROVES IT CLEAN.
//
// Both halves are the assertion. Freeing the slot is the point of reclaim, and
// routing it through the release gate is what stops reclaim becoming the one
// path by which an unproved connection reaches the next developer.
func TestReclaim_AnIdleEmptyHolderYieldsItsBackendThroughTheGate(t *testing.T) {
	s, c := reclaimHolder(t, 4)
	e := &Engine{sessions: registryHolding(s)}

	if !e.reclaimOneIdleBackend(context.Background()) {
		t.Fatal("an idle holder with no transaction and nothing on the backend was not reclaimed")
	}

	s.mu.Lock()
	held := s.pc
	s.mu.Unlock()
	if held != nil {
		t.Error("the reclaimed session still holds a backend, so the slot was never freed")
	}
	if c.released != 1 {
		t.Errorf("the backend was released %d times, want 1 — reclaim must return it to the pool", c.released)
	}
	if len(c.ran) == 0 {
		t.Error("nothing was dispatched on the reclaimed backend: it went back to the pool " +
			"without the reset that proves it carries none of the holder's state")
	}
}

// EVERY HOLDER WHO WOULD NOTICE KEEPS THEIR BACKEND.
//
// These are not degrees of risk, they are four separate ways the holder finds
// out: a rollback they did not ask for, a cancelled statement, or a 26000 on
// the next Bind for an object they correctly believe they created. A reclaim
// that fires in any of these states is silent data loss, so the cell asserts
// the refusal rather than the recovery.
func TestReclaim_RefusesAnyHolderWhoCouldTell(t *testing.T) {
	withObjects := func(mutate func(*extObjects)) func(*session) {
		return func(s *session) {
			s.ext = &extObjects{
				statements: map[string]*extStatement{},
				portals:    map[string]*extPortal{},
			}
			mutate(s.ext)
		}
	}

	for _, tc := range []struct {
		name  string
		state func(*session)
		why   string
	}{
		{
			name:  "a transaction is open",
			state: func(s *session) { s.tx = stubTxConn{} },
			why:   "reclaiming rolls back work the holder never abandoned",
		},
		{
			name:  "a request is in flight",
			state: func(s *session) { s.busy = true },
			why:   "the request is within its bounds and may not be cancelled for capacity",
		},
		{
			name: "a prepared statement lives on the backend",
			state: withObjects(func(x *extObjects) {
				x.statements["s1"] = &extStatement{}
			}),
			why: "the holder's next Bind meets 26000 for an object it did create",
		},
		{
			name: "a portal lives on the backend",
			state: withObjects(func(x *extObjects) {
				x.portals["p1"] = &extPortal{}
			}),
			why: "the holder's next Execute meets 34000 for a portal it did open",
		},
		{
			name: "a queued Close is unacknowledged",
			state: withObjects(func(x *extObjects) {
				x.pendingCloses = []objectRef{{}}
			}),
			why: "the target may still hold the object, so the store is not yet empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := reclaimHolder(t, 4)
			tc.state(s)
			e := &Engine{sessions: registryHolding(s)}

			if e.reclaimOneIdleBackend(context.Background()) {
				t.Fatalf("the backend was reclaimed while %s — %s", tc.name, tc.why)
			}
			s.mu.Lock()
			held := s.pc
			s.mu.Unlock()
			if held == nil {
				t.Error("the holder lost its backend even though the reclaim reported none was taken")
			}
			if c.released != 0 {
				t.Errorf("the backend was released %d times, want 0", c.released)
			}
		})
	}
}

// WITH NOBODY TO RECLAIM FROM, RECLAIM SAYS SO RATHER THAN GUESSING.
func TestReclaim_ReportsNothingWhenEveryHolderIsBusy(t *testing.T) {
	busy, _ := reclaimHolder(t, 4)
	busy.busy = true
	e := &Engine{sessions: registryHolding(busy)}
	if e.reclaimOneIdleBackend(context.Background()) {
		t.Fatal("reclaim reported it freed a slot when every holder was ineligible")
	}
	if (&Engine{}).reclaimOneIdleBackend(context.Background()) {
		t.Fatal("an engine with no session registry reported a reclaim")
	}
}

// A REQUEST THAT CAN BE SERVED BY RECLAIMING IS NOT MADE TO WAIT.
//
// This is the seam between the two halves: the queue is for real scarcity, and
// a slot merely being held is not scarcity. If this cell fails, every request
// arriving at a saturated instance waits the full ninety seconds for capacity
// that was available the whole time.
func TestAcquireOrWait_ReclaimsRatherThanQueueingWhenASlotIsMerelyHeld(t *testing.T) {
	l := newQueuedLedger(t, 2) // ordinary allowance 1
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatalf("taking the only ordinary slot: %v", err)
	}

	var calls atomic.Int32
	l.reclaimIdle = func(context.Context) bool {
		calls.Add(1)
		held.Release()
		return true
	}

	done := make(chan *Permit, 1)
	go func() {
		p, werr := l.AcquireOrWait(context.Background(), 2)
		if werr != nil {
			done <- nil
			return
		}
		done <- p
	}()

	select {
	case p := <-done:
		if p == nil {
			t.Fatal("the request was refused although a held slot could have been reclaimed")
		}
		p.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("the request queued instead of reclaiming a slot that was merely held")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("reclaim was attempted %d times, want exactly 1 — one request's pressure "+
			"must not become a sweep of every idle session", n)
	}
}

// A RECLAIM THAT FREED NOTHING FALLS THROUGH TO THE QUEUE INSTEAD OF SPINNING.
//
// The reclaimer reports what it did, and what it did can be overtaken: another
// request may take the freed slot first. That is not a failure and must not
// become a retry loop, because a loop here turns contention into a spin that
// burns a core while the queue — which is the correct answer for sustained
// pressure — never gets used.
func TestAcquireOrWait_AnOvertakenReclaimQueuesRatherThanRetrying(t *testing.T) {
	l := newQueuedLedger(t, 2)
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatalf("taking the only ordinary slot: %v", err)
	}
	defer held.Release()

	var calls atomic.Int32
	l.reclaimIdle = func(context.Context) bool {
		calls.Add(1)
		return true // claims success, frees nothing: somebody else got there first
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, werr := l.AcquireOrWait(ctx, 2); done <- werr }()

	deadline := time.Now().Add(5 * time.Second)
	for l.queue.Depth() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if l.queue.Depth() != 1 {
		t.Fatalf("queue depth = %d, want 1 — an overtaken reclaim must queue, not spin", l.queue.Depth())
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("reclaim was attempted %d times, want 1", n)
	}
	cancel()
	if werr := <-done; !errors.Is(werr, context.Canceled) {
		t.Errorf("the queued request returned %v, want the caller's cancellation", werr)
	}
}

// RECLAIM IS NOT TRIED WHEN EVERY SLOT IS INSIDE A TRANSACTION.
//
// The pre-enqueue refusal comes first by construction: there is nothing to
// reclaim in that state, and attempting it would walk every session to
// rediscover what the ledger already knows.
func TestAcquireOrWait_AllCapacityInTransactionSkipsReclaimEntirely(t *testing.T) {
	l := newQueuedLedger(t, 2)
	held, err := l.AcquireOrWait(context.Background(), 1)
	if err != nil {
		t.Fatalf("taking the only ordinary slot: %v", err)
	}
	defer held.Release()

	l.inTransaction = func() int { return 1 }
	var calls atomic.Int32
	l.reclaimIdle = func(context.Context) bool { calls.Add(1); return false }

	_, werr := l.AcquireOrWait(context.Background(), 2)
	if !errors.Is(werr, ErrAllCapacityInTransaction) {
		t.Fatalf("got %v, want the pre-enqueue refusal", werr)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("reclaim was attempted %d times while every slot was in a transaction, want 0", n)
	}
}
