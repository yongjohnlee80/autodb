package exec

import (
	"context"
	"sync"
	"testing"
	"time"
)

// demandEngineWith builds an engine whose registry holds these wire sessions,
// each already holding a lease on its own connID.
//
// SEEDED DIRECTLY, NOT THROUGH THE QUEUE. The arrival path is the seam these
// cells are about, so putting the preconditions through it would mean a change
// that breaks arrival dispatch breaks the setup before the assertion runs --
// which is how a control ends up INVALID instead of red.
func demandEngineWith(t *testing.T, leaseCap int, holders ...*session) *Engine {
	t.Helper()
	e := New(nil, nil)
	e.sessions.leaseCap = leaseCap
	for _, s := range holders {
		if err := e.sessions.admitWithLease(s, s.connID, 0); err != nil {
			t.Fatalf("seeding holder %s: %v", s.id, err)
		}
		e.sessions.genSeq++
		s.gen = e.sessions.genSeq
	}
	return e
}

// unofferedHolder is a wire session that is busy and publishing no offer, so
// nothing can reclaim it yet -- the state every holder is in at the instant
// this bug used to strand a request.
func unofferedHolder(id string, userID, connID int64, idleSince time.Time) *session {
	s := demandHolder(id, userID, connID, idleSince)
	s.busy = true
	s.recvToken = 0
	return s
}

// A HOLDER THAT BECOMES ASKABLE ANSWERS A REQUEST THAT ALREADY ASKED AND FAILED.
//
// THIS IS THE LIVENESS BUG. Demand used to be attempted exactly once, at
// arrival. If every holder was busy, transacting or between offers in that
// instant, nothing was reserved -- and because a wire lease is held for a
// session's whole lifetime, nothing releases on its own. No later event asked
// again, so the queued request sat out its entire bound beside a holder that
// had been reclaimable for almost all of it.
//
// No second request arrives in this cell. Exactly one holder becomes askable,
// and that alone must be enough.
func TestDemandRetry_AHolderThatBecomesAskableServesTheRequestThatAlreadyAsked(t *testing.T) {
	now := time.Now()
	holder := unofferedHolder("busy-at-the-moment-of-arrival", 1, 7, now.Add(-time.Hour))
	e := demandEngineWith(t, 1, holder)
	r := e.sessions
	seen := queuedAt(r)

	knocked := make(chan struct{}, 1)
	holder.wake = func() {
		select {
		case knocked <- struct{}{}:
		default:
		}
	}

	waiterDone := make(chan error, 1)
	waiter := schedSession("arrived-while-everything-was-busy", 2, 7)
	go func() { waiterDone <- r.admitWithLeaseOrWait(context.Background(), waiter, 7, 0) }()
	awaitSeq(t, seen)

	// The arrival ask has happened and found nothing. That is the premise.
	if holder.get() != sessOpen {
		t.Fatal("the holder was reserved although it was busy and offering nothing; the " +
			"premise of this cell is that the arrival ask finds nobody")
	}

	// Now the holder finishes its work and goes back to reading its client --
	// the ordinary transition, and the only event on this target.
	holder.mu.Lock()
	holder.busy = false
	holder.mu.Unlock()
	if tok := e.OfferReceive(holder.id); tok == 0 {
		t.Fatal("the holder could not publish a receive offer, so this cell cannot reach " +
			"the transition it is about")
	}

	select {
	case <-knocked:
	case <-time.After(5 * time.Second):
		t.Fatal("no holder was asked to leave when one became askable; a request that is " +
			"already queued will now wait out its whole bound beside an idle holder, " +
			"because nothing releases a wire lease on its own")
	}

	// The owner accepts and tears down, which is what actually frees the lease.
	// Removed directly rather than through FinishDemandReclaim: finalisation
	// audits, and this engine has no audit service. What this cell is about is
	// the ask that produced the knock -- the finalisation path has its own
	// cells, and borrowing it here would only add a way for this one to fail
	// for a reason it is not about.
	r.remove(holder)

	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("the queued request was not served by the lease it caused to be "+
				"reclaimed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reclaimed lease never reached the request that was waiting for it")
	}
}

// ONE WAITER COSTS ONE SESSION, NOT EVERY IDLE ONE.
//
// The retry fires on every offer, and offers are ordinary and frequent. Without
// counting what is already coming, a single queued request would end every
// holder on the target as each went back to reading -- turning a fix for one
// stranded request into an outage for everybody else on that connection.
func TestDemandRetry_OneWaitingRequestReclaimsOneHolder(t *testing.T) {
	now := time.Now()
	holders := []*session{
		unofferedHolder("holder-a", 1, 7, now.Add(-3*time.Hour)),
		unofferedHolder("holder-b", 2, 7, now.Add(-2*time.Hour)),
		unofferedHolder("holder-c", 3, 7, now.Add(-1*time.Hour)),
	}
	e := demandEngineWith(t, len(holders), holders...)
	r := e.sessions
	seen := queuedAt(r)

	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- r.admitWithLeaseOrWait(context.Background(),
			schedSession("the-only-request-waiting", 9, 7), 7, 0)
	}()
	awaitSeq(t, seen)

	// Every holder goes back to reading, one after another.
	for _, h := range holders {
		h.mu.Lock()
		h.busy = false
		h.mu.Unlock()
		e.OfferReceive(h.id)
	}

	reserved := 0
	for _, h := range holders {
		if h.get() != sessOpen {
			reserved++
		}
	}
	if reserved != 1 {
		t.Errorf("%d of %d holders were reserved for one waiting request; demand must ask "+
			"for what is missing, not for everything that happens to be askable",
			reserved, len(holders))
	}
	_ = waiterDone
}

// A REQUEST THAT HAS GONE LEAVES NO DEMAND BEHIND IT.
//
// The claim outlives the arrival that made it, so it has to be given back on
// every route out of the line -- served, cancelled, expired, refused, or the
// target being drained. A claim left behind makes the target look permanently
// short, and the next holder to go idle is ended for somebody who is no longer
// there.
func TestDemandRetry_ACancelledRequestLeavesNoDemandBehind(t *testing.T) {
	now := time.Now()
	holder := unofferedHolder("holder", 1, 7, now.Add(-time.Hour))
	e := demandEngineWith(t, 1, holder)
	r := e.sessions
	seen := queuedAt(r)

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		gone <- r.admitWithLeaseOrWait(ctx, schedSession("gives-up", 2, 7), 7, 0)
	}()
	awaitSeq(t, seen)

	cancel()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled request never returned")
	}

	if r.demandOutstanding(7) {
		t.Error("the target still reports unanswered demand after the only request waiting " +
			"on it gave up")
	}

	// And the proof that matters: a holder going idle now ends nobody.
	holder.mu.Lock()
	holder.busy = false
	holder.mu.Unlock()
	e.OfferReceive(holder.id)

	if holder.get() != sessOpen {
		t.Error("an idle holder was ended for a request that had already gone; a stale " +
			"demand claim costs somebody their session for nobody's benefit")
	}
}

// ONE WAITER IS SPENT ONCE, WITH BOTH CALLERS HELD INSIDE THE STALE WINDOW.
//
// THIS CELL HAS BEEN WRONG TWICE, AND BOTH VERSIONS WERE WRONG ABOUT WHERE THE
// RACE LIVES. The first opened both offers before starting its goroutines, so
// one holder was already reserved and only one candidate remained; its launch
// gate was a starting pistol, not a barrier. The second put a seam at the claim
// boundary, which cannot hold two callers at once -- the candidate walk takes
// each candidate's mutex in turn, so the second caller parks on a lock rather
// than deciding anything -- and then inferred that non-arrival from a fixed
// sleep, which the test convention forbids for exactly this reason.
//
// The window is earlier. Two callers can both pass pressDemand's advisory
// outstanding check before either selects anything, and both then go on to
// select. That is the window the old design lost a session in, and it is the
// one both callers can genuinely be held inside. Afterwards the candidate walk
// may serialise as it likes: the first claims and reserves A, and the second --
// which passed the stale check when the unit looked free -- reaches B and must
// be refused there.
//
// The barrier is observable. Nothing here sleeps to infer a state.
func TestDemandRetry_ConcurrentOffersSpendOneWaiterOnce(t *testing.T) {
	now := time.Now()
	holders := []*session{
		unofferedHolder("holder-a", 1, 7, now.Add(-2*time.Hour)),
		unofferedHolder("holder-b", 2, 7, now.Add(-time.Hour)),
	}
	e := demandEngineWith(t, len(holders), holders...)
	r := e.sessions
	seen := queuedAt(r)

	go func() {
		_ = r.admitWithLeaseOrWait(context.Background(),
			schedSession("the-only-request-waiting", 9, 7), 7, 0)
	}()
	awaitSeq(t, seen)

	// Receive state published DIRECTLY. Going through OfferReceive would press
	// demand and reserve a holder before the barrier was armed, which is what
	// hollowed out the first version of this cell.
	for _, h := range holders {
		h.mu.Lock()
		h.busy = false
		h.tokenSeq++
		h.recvToken = h.tokenSeq
		h.mu.Unlock()
	}

	var passed sync.WaitGroup
	passed.Add(len(holders))
	release := make(chan struct{})
	r.hookAfterDemandCheck = func() {
		passed.Done()
		<-release
	}

	var done sync.WaitGroup
	for range holders {
		done.Add(1)
		go func() {
			defer done.Done()
			r.pressDemand(7)
		}()
	}

	bothPassed := make(chan struct{})
	go func() { passed.Wait(); close(bothPassed) }()
	select {
	case <-bothPassed:
	case <-time.After(5 * time.Second):
		t.Fatal("both callers never passed the outstanding check together, so the stale " +
			"window was never entered and this cell proves nothing")
	}
	// Both are now past the advisory check, each believing a unit is free.
	close(release)
	done.Wait()

	reserved := 0
	for _, h := range holders {
		if h.get() != sessOpen {
			reserved++
		}
	}
	if reserved != 1 {
		t.Errorf("%d sessions reserved for one waiter after concurrent offers, want 1 — "+
			"a demand unit checked in one critical section and spent in another is read "+
			"as unspent by everybody who looks before the first one records it", reserved)
	}
	if n := r.promisedCount(7); n != 1 {
		t.Errorf("%d promises recorded for one waiter, want 1", n)
	}
	open := 0
	for _, h := range holders {
		if h.get() == sessOpen {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d holders left open, want 1 — the second caller passed the stale check "+
			"and must still have been refused at the claim", open)
	}
}

// AN ASK THAT NOBODY IS WAITING ON RESERVES NOBODY.
//
// The claim outlives the check, so a candidate committing after the waiter has
// gone would end a session for a request that no longer exists. Proved at the
// commit point rather than at the ask, because that is where the decision is
// now made.
func TestDemandRetry_AnOfferAfterTheWaiterHasGoneReservesNobody(t *testing.T) {
	now := time.Now()
	holder := unofferedHolder("holder", 1, 7, now.Add(-time.Hour))
	e := demandEngineWith(t, 1, holder)
	r := e.sessions
	seen := queuedAt(r)

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() { gone <- r.admitWithLeaseOrWait(ctx, schedSession("gives-up", 2, 7), 7, 0) }()
	awaitSeq(t, seen)

	cancel()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled request never returned")
	}

	// The candidate becomes eligible only now, after the waiter has left.
	holder.mu.Lock()
	holder.busy = false
	holder.mu.Unlock()
	e.OfferReceive(holder.id)

	// And the reservation is attempted directly too, so the refusal is proved
	// at the commit point and not merely at pressDemand's early check.
	if v, ok := r.reserveDemandVictim(7, now); ok {
		t.Errorf("reserved %q for a request that had already gone; the claim is what "+
			"authorises ending somebody's session, and there was none to spend", v.s.id)
	}
	if holder.get() != sessOpen {
		t.Error("an idle holder was ended although nobody was waiting for its lease")
	}
	if n := r.promisedCount(7); n != 0 {
		t.Errorf("%d promises recorded with no waiter, want 0", n)
	}
}
