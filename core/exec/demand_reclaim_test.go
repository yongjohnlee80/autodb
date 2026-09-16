package exec

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// WHAT THESE CELLS ARE FOR: demand reclamation ends a real person's session so
// that somebody else's request can proceed. Every cell below is one way that
// could go wrong in a manner nobody would notice until a developer complained
// that their connection vanished — chosen while they were mid-transaction,
// chosen twice, or chosen and then never told.

func demandHolder(id string, userID, connID int64, idleSince time.Time) *session {
	s := &session{id: SessionID(id), userID: userID, connID: connID, lastUsed: idleSince}
	s.wire = true
	s.state.Store(int32(sessOpen))
	s.wake = func() {}
	s.tokenSeq++
	s.recvToken = s.tokenSeq
	return s
}

func demandRegistry(t *testing.T, holders ...*session) *sessionRegistry {
	t.Helper()
	r := newSessionRegistry(64, 64)
	r.leaseCap = 1
	for _, s := range holders {
		r.byID[s.id] = s
		s.reservation = reservation{LeaseConn: s.connID}
		r.genSeq++
		s.gen = r.genSeq
	}
	return r
}

// ONLY A HOLDER WHO CAN BE TOLD, AND WHO LOSES NOTHING, IS CHOSEN.
//
// Each row is somebody who would be harmed rather than merely inconvenienced,
// or somebody nobody could explain it to. A predicate that admits any of them
// turns a capacity fix into a client losing work without being told why.
func TestDemandReclaim_OnlyAnUntroubledIdleHolderIsChosen(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		spoil func(*session)
		why   string
	}{
		{
			name:  "a transaction is open",
			spoil: func(s *session) { s.tx = stubTxConn{} },
			why:   "ending it rolls back work the holder never abandoned",
		},
		{
			name:  "a request is in flight",
			spoil: func(s *session) { s.busy = true },
			why:   "the request is inside its bounds and may not be cancelled for capacity",
		},
		{
			name:  "the session is already closing",
			spoil: func(s *session) { s.state.Store(int32(sessClosing)) },
			why:   "something else already owns this teardown",
		},
		{
			name:  "its owner is not currently able to act",
			spoil: func(s *session) { s.recvToken = 0 },
			why: "a notice posted now would be erased by the owner re-arming its own " +
				"deadline, stranding the lease this was meant to free",
		},
		{
			name:  "it is not a front-door session",
			spoil: func(s *session) { s.wire = false },
			why:   "there is no client loop to frame it, and nobody to tell",
		},
		{
			name:  "nothing can reach its client",
			spoil: func(s *session) { s.wake = nil; s.recvToken = 0 },
			why:   "ending a session nobody can explain to is worse than not reclaiming it",
		},
		{
			// ISOLATED FROM THE ROW ABOVE ON PURPOSE. Clearing both hides this
			// shape: an offer that is open while no knock exists is a promise
			// the session cannot keep, and believing it strands the lease
			// behind a session that has been closed and serves nobody.
			name:  "it offers to receive but nothing can knock",
			spoil: func(s *session) { s.wake = nil },
			why:   "nothing would make the owner's read return, so the notice sits unread",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
			tc.spoil(s)
			r := demandRegistry(t, s)
			if _, ok := r.reserveDemandVictim(7, now); ok {
				t.Fatalf("a holder was chosen while %s — %s", tc.name, tc.why)
			}
			if s.get() != sessOpen {
				t.Error("a holder that was not chosen was left reserved for teardown, so " +
					"it now serves nobody and nothing is coming to release its lease")
			}
		})
	}

	t.Run("idle, quiet and reachable", func(t *testing.T) {
		s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
		r := demandRegistry(t, s)
		v, ok := r.reserveDemandVictim(7, now)
		if !ok {
			t.Fatal("an idle holder with no transaction, no request in flight and a live owner was not chosen")
		}
		if v.s != s {
			t.Error("a different session was chosen")
		}
		if s.get() != sessClosing {
			t.Error("the chosen session was not reserved for teardown, so a statement " +
				"arriving now would run on a session already promised to somebody else")
		}
		if v.notice.IdleFor < time.Hour {
			t.Errorf("idle time recorded as %s, want at least an hour — it is the justification "+
				"for ending somebody's session and belongs in the record", v.notice.IdleFor)
		}
	})
}

// THE LONGEST-SILENT HOLDER IS CHOSEN.
//
// Every candidate is equally reclaimable by the predicate, but they are not
// equally cheap to end: choosing arbitrarily would sometimes end the session of
// somebody who paused for a moment while an hour-idle one sat beside it.
func TestDemandReclaim_TheLongestSilentHolderIsChosen(t *testing.T) {
	now := time.Now()
	recent := demandHolder("paused-a-moment-ago", 1, 7, now.Add(-2*time.Second))
	ancient := demandHolder("away-for-an-hour", 2, 7, now.Add(-time.Hour))
	middling := demandHolder("away-for-a-minute", 3, 7, now.Add(-time.Minute))
	r := demandRegistry(t, recent, ancient, middling)

	v, ok := r.reserveDemandVictim(7, now)
	if !ok {
		t.Fatal("no holder was chosen")
	}
	if v.s != ancient {
		t.Errorf("chose %q, want the holder that had been silent longest", v.s.id)
	}
}

// THE CLAIM IS SINGLE USE, SO A SESSION CANNOT BE ENDED TWICE.
//
// Demand, the client's own disconnect, an operator's connection delete and the
// idle reaper can all decide to end the same session in the same instant.
// Exactly one may frame it, record it and release its lease; a second terminal
// frame is at best noise and at worst a write into a socket another goroutine
// is closing.
func TestDemandReclaim_TheReservationIsSingleUseUnderContention(t *testing.T) {
	now := time.Now()
	s := demandHolder("contended", 1, 7, now.Add(-time.Hour))
	r := demandRegistry(t, s)

	const racers = 8
	var wins atomic.Int32
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if _, ok := r.reserveDemandVictim(7, now); ok {
				wins.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	if n := wins.Load(); n != 1 {
		t.Errorf("%d of %d contenders reserved the same session, want exactly 1 — more than "+
			"one terminal owner means a client is framed twice and its lease released twice", n, racers)
	}
}

// THE RESERVATION IS THE SAME ONE EVERY OTHER TEARDOWN CONTENDS FOR.
//
// Demand must not have a claim of its own beside the ordinary close, or the
// reaper, an operator's connection delete and the client's own disconnect can
// each decide to end the same session without knowing about this one.
func TestDemandReclaim_OrdinaryCloseAndDemandContendForOneReservation(t *testing.T) {
	now := time.Now()
	s := demandHolder("contended", 1, 7, now.Add(-time.Hour))
	r := demandRegistry(t, s)

	// An ordinary close gets there first.
	if !s.beginClose("", "client-closed") {
		t.Fatal("the ordinary close could not reserve an open session")
	}
	if _, ok := r.reserveDemandVictim(7, now); ok {
		t.Error("demand reserved a session an ordinary close was already ending — the client " +
			"would be framed twice and the lease released twice")
	}
}

// A SESSION THAT BECOMES ACTIVE IS NOT RESERVED.
//
// The predicate and the reservation happen under one hold of the session's
// lock, so a statement arriving in between cannot be overtaken by a decision
// that was true a moment earlier.
func TestDemandReclaim_ASessionThatBecameActiveIsNotReserved(t *testing.T) {
	now := time.Now()
	busy := demandHolder("started-a-query", 1, 7, now.Add(-time.Hour))
	busy.busy = true
	quiet := demandHolder("still-idle", 2, 7, now.Add(-time.Minute))
	r := demandRegistry(t, busy, quiet)

	// The busy one has been idle longest, so it is tried first and must be
	// passed over rather than reserved.
	v, ok := r.reserveDemandVictim(7, now)
	if !ok {
		t.Fatal("no holder was reserved, though one was idle and quiet")
	}
	if v.s != quiet {
		t.Errorf("reserved %q, want the holder that was not running a statement", v.s.id)
	}
	if busy.get() != sessOpen {
		t.Error("the busy holder was left reserved for teardown")
	}
}

// A CLAIM CANNOT END WHICHEVER SESSION CAME AFTER THE ONE IT WAS TAKEN AGAINST.
//
// A claim taken against a session that ends on its own before the wake arrives
// must not be honoured later. Without the generation, a slow wake would end a
// stranger's session — a developer disconnected because somebody else's session
// was selected a moment earlier.
func TestDemandReclaim_AStaleGenerationCannotEndAReplacementSession(t *testing.T) {
	now := time.Now()
	victim := demandHolder("chosen", 1, 7, now.Add(-time.Hour))
	r := demandRegistry(t, victim)
	v, ok := r.reserveDemandVictim(7, now)
	if !ok {
		t.Fatal("no holder was chosen")
	}

	// The chosen session ends on its own, and another takes its place.
	r.remove(victim)
	replacement := demandHolder("chosen", 2, 7, now)
	r2 := demandRegistry(t, replacement)
	if replacement.gen == v.notice.Gen {
		t.Fatal("a claim taken against one session is valid against its replacement — a slow " +
			"wake would end a session that was never selected")
	}
	e := &Engine{sessions: r2}
	if e.FinishDemandReclaim(context.Background(), "chosen", v.notice.Gen, true) {
		t.Error("a stale claim ended a replacement session")
	}
}

// THE FREED LEASE GOES THROUGH THE LINE, NOT TO WHOEVER TRIGGERED THE RECLAIM.
//
// Handing it straight back to the requester would make demand a way to jump the
// queue, and a queue with a bypass is not a queue. The request that waited
// longest gets it, even though a later arrival is what caused it to be freed.
func TestDemandReclaim_TheFreedLeaseGoesToTheLongestWaiter(t *testing.T) {
	r := schedRegistry(t, 1)
	seen := queuedAt(r)

	holder := schedSession("holder", 1, 7)
	if err := r.admitWithLeaseOrWait(context.Background(), holder, 7, 0); err != nil {
		t.Fatal(err)
	}

	first := schedSession("asked-first", 2, 7)
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.admitWithLeaseOrWait(context.Background(), first, 7, 0) }()
	awaitSeq(t, seen)

	// The second arrival is what triggers the reclaim.
	var once sync.Once
	r.onDemand = func(int64) bool {
		once.Do(func() { r.remove(holder) })
		return true
	}
	second := schedSession("asked-second-and-caused-the-reclaim", 3, 7)
	secondDone := make(chan error, 1)
	go func() { secondDone <- r.admitWithLeaseOrWait(context.Background(), second, 7, 0) }()

	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the longest waiter was not served: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reclaimed lease did not reach the request that had waited longest")
	}
	select {
	case err := <-secondDone:
		t.Fatalf("the request that caused the reclaim took the lease it freed (err=%v) — "+
			"demand must not be a way around the line", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// THE OFFER, THE RESERVATION AND THE NOTICE ARE ONE OPERATION.
//
// THIS IS THE CELL FOR A RACE THAT USED TO END SOMEBODY'S SESSION SILENTLY.
// The receive window used to live in the front door while the engine kept a
// flag, so the scheduler could read the flag as open, reserve a session for
// termination, and only then discover the window had closed because the
// client's own frame had arrived. The session was ended with no way to tell it
// — a race the client had won, repaired by terminating them.
//
// Now a reservation is only ever taken against a live offer, under the lock
// that holds both, and the notice is published in the same breath. Retiring the
// offer first means no reservation can follow it.
func TestDemandReclaim_ARetiredOfferCannotBeReserved(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
	r := demandRegistry(t, s)
	e := &Engine{sessions: r}

	// The owner's read returns first: the offer closes.
	if _, woken := e.RetireReceive(s.id, s.recvToken); woken {
		t.Fatal("a notice was delivered when none had been published")
	}
	if _, ok := r.reserveDemandVictim(7, now); ok {
		t.Error("a session was reserved after its owner had stopped listening; it would be " +
			"terminated with no way to tell it, having lost a race it had in fact won")
	}
	if s.get() != sessOpen {
		t.Error("the session was left reserved for teardown after a failed selection")
	}
}

// A RESERVED SESSION'S OWNER IS ALWAYS TOLD.
//
// The reverse of the cell above: if the reservation committed, the notice is
// there for the owner to find, because both happened under one lock.
func TestDemandReclaim_AReservationAlwaysCarriesItsNotice(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now.Add(-90*time.Minute))
	r := demandRegistry(t, s)
	e := &Engine{sessions: r}
	token := s.recvToken

	v, ok := r.reserveDemandVictim(7, now)
	if !ok {
		t.Fatal("an eligible holder was not reserved")
	}
	n, woken := e.RetireReceive(s.id, token)
	if !woken {
		t.Fatal("the reserved session's owner found no notice, so it would dispatch the " +
			"client's next frame on a session already promised to somebody else")
	}
	if n.Gen != v.notice.Gen || n.ID != s.id {
		t.Errorf("notice = %+v, want the one the reservation published", n)
	}
	if n.IdleFor < 89*time.Minute {
		t.Errorf("idle time = %s, want roughly ninety minutes — it is the justification for "+
			"ending somebody's session", n.IdleFor)
	}
}

// A STALE TOKEN RETIRES NOTHING.
//
// It would mean the call belongs to an offer that has already been closed, and
// honouring it would let one read's outcome close a later read's window.
func TestDemandReclaim_AStaleReceiveTokenIsIgnored(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now)
	r := demandRegistry(t, s)
	e := &Engine{sessions: r}

	stale := s.recvToken
	fresh := e.OfferReceive(s.id)
	if fresh == stale {
		t.Fatal("a new offer reused the retired offer's token")
	}
	if _, ok := r.reserveDemandVictim(7, now); !ok {
		t.Fatal("the holder was not reserved")
	}
	if _, woken := e.RetireReceive(s.id, stale); woken {
		t.Error("a stale token took the notice, so the offer it belonged to could close a " +
			"window that had already been reopened")
	}
	if _, woken := e.RetireReceive(s.id, fresh); !woken {
		t.Error("the live token did not find the notice")
	}
}

// ONE ENDING LEAVES ONE RECORD, CARRYING WHAT ONLY THE OWNER KNEW.
//
// THIS IS THE CELL FOR A TRAIL THAT DOUBLE-COUNTED. The teardown already
// records the session closing with the reclamation as its reason; the front
// door used to write an event of its own beside it, so one ending produced two
// entries and anyone counting reclamations counted them twice. The two facts
// only the owner knows — how long the session had been silent, and whether its
// client actually received the frame — now complete that single record instead
// of justifying a second one.
func TestDemandReclaim_TheOneRecordCarriesIdleTimeAndWhetherTheClientWasTold(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered bool
		wants     string
	}{
		{"the client received the frame", true, "the client was told"},
		{"the client had already gone", false, "could not be told"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := demandHolder("holder", 1, 7, now.Add(-2*time.Hour))
			r := demandRegistry(t, s)
			e := &Engine{sessions: r, now: func() time.Time { return now }}

			if _, ok := r.reserveDemandVictim(7, now); !ok {
				t.Fatal("the holder was not reserved")
			}

			s.mu.Lock()
			before := s.closeWhy
			s.mu.Unlock()
			if before != ReasonDemandReclaimed {
				t.Fatalf("the reservation recorded %q, want the reclamation reason", before)
			}

			// Finalisation completes that reason rather than adding a record.
			e.completeDemandReason(s, tc.delivered)

			s.mu.Lock()
			why := s.closeWhy
			s.mu.Unlock()
			if !strings.HasPrefix(why, ReasonDemandReclaimed) {
				t.Errorf("close reason = %q, want it to still name the reclamation", why)
			}
			if !strings.Contains(why, "2h0m0s") {
				t.Errorf("close reason = %q, want it to carry how long the session had been "+
					"silent — that is the justification for ending it", why)
			}
			if !strings.Contains(why, tc.wants) {
				t.Errorf("close reason = %q, want it to say %q", why, tc.wants)
			}
		})
	}
}

// AN OFFER IS NEVER ISSUED WITHOUT A KNOCK TO GO WITH IT.
//
// The two guards protect different mistakes and both are wanted: this one stops
// a live offer existing at all, and the eligibility predicate stops a
// reservation committing against one anyway.
func TestDemandReclaim_NoOfferIsIssuedWithoutAKnock(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now)
	s.wake = nil
	r := demandRegistry(t, s)
	e := &Engine{sessions: r}

	if token := e.OfferReceive(s.id); token != 0 {
		t.Errorf("an offer was issued as token %d with no knock registered — nothing could "+
			"make this owner's read return, so its lease would be stranded", token)
	}
}

// THE NEXT ELIGIBLE HOLDER IS TAKEN WHEN ONE IS PASSED OVER.
//
// Passing over an ineligible candidate must not end the search, or one
// unreachable session would protect every other holder on the target from ever
// being reclaimed.
func TestDemandReclaim_APassedOverHolderDoesNotEndTheSearch(t *testing.T) {
	now := time.Now()
	unreachable := demandHolder("offers-but-cannot-be-knocked", 1, 7, now.Add(-time.Hour))
	unreachable.wake = nil // its offer stands, but nothing can knock
	usable := demandHolder("reachable", 2, 7, now.Add(-time.Minute))
	r := demandRegistry(t, unreachable, usable)

	v, ok := r.reserveDemandVictim(7, now)
	if !ok {
		t.Fatal("no holder was reserved, though a reachable one was available")
	}
	if v.s != usable {
		t.Errorf("reserved %q, want the holder that can actually be told", v.s.id)
	}
	if unreachable.get() != sessOpen {
		t.Error("the unreachable holder was left reserved for teardown, so its lease is now " +
			"held by a session that is closing and serves nobody")
	}
}

// THE RECORD KEEPS THE SILENCE THAT JUSTIFIED THE DECISION, NOT THE SILENCE AT
// THE TIME OF WRITING.
//
// THIS IS THE CELL FOR A NUMBER THAT WOULD HAVE LIED IN ITS OWN FAVOUR. The
// ending is written after the knock, the frame and a bounded flush, so
// measuring the silence again at that point means a slow or unresponsive client
// inflates the very figure offered as the reason for ending it. The worse the
// client behaves, the more justified the decision looks.
func TestDemandReclaim_TheRecordKeepsTheIdleTimeTheDecisionWasMadeOn(t *testing.T) {
	at := time.Now()
	s := demandHolder("holder", 1, 7, at.Add(-30*time.Minute))
	r := demandRegistry(t, s)
	clock := at
	e := &Engine{sessions: r, now: func() time.Time { return clock }}

	if _, ok := r.reserveDemandVictim(7, clock); !ok {
		t.Fatal("the holder was not reserved")
	}

	// Everything after selection takes time: the knock, the frame, the flush.
	clock = at.Add(9 * time.Hour)
	e.completeDemandReason(s, true)

	s.mu.Lock()
	why := s.closeWhy
	s.mu.Unlock()
	if !strings.Contains(why, "30m0s") {
		t.Errorf("close reason = %q, want the thirty minutes of silence the decision rested "+
			"on", why)
	}
	if strings.Contains(why, "9h") {
		t.Error("the record measured the silence again at the time of writing, so a client " +
			"that was slow to accept its frame inflated the justification for ending it")
	}
}
