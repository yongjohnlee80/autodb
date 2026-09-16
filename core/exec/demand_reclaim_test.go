package exec

import (
	"context"
	"encoding/json"
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
	// A COMPLETE ENOUGH SESSION TO BE TORN DOWN, not just to be judged.
	//
	// Without a context the teardown path nil-derefs, so a control that lets a
	// reclamation proceed further than it should makes the cell PANIC instead
	// of failing its assertion -- which the runner classifies as INVALID,
	// correctly, because a crash proves nothing about the thing being claimed.
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		id: SessionID(id), userID: userID, connID: connID, lastUsed: idleSince,
		ctx: ctx, cancel: cancel,
	}
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
			before := s.get()
			if _, ok := r.reserveDemandVictim(7, now); ok {
				t.Fatalf("a holder was chosen while %s — %s", tc.name, tc.why)
			}
			// COMPARED AGAINST WHAT IT WAS, not against open: one of these rows
			// spoils the session by closing it, and asserting "still open"
			// there would fail for the very reason the row exists.
			if s.get() != before {
				t.Errorf("the failed selection changed the holder's state from %v to %v; a "+
					"holder left reserved for teardown serves nobody and nothing is "+
					"coming to release its lease", before, s.get())
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

	// The chosen session ends on its own, and another takes its place IN THE
	// SAME REGISTRY — which is the only way this means anything. Building the
	// replacement in a fresh registry restarts the generation counter, so the
	// cell would have been comparing two independent sequences and would have
	// passed for a reason that has nothing to do with the guard.
	r.remove(victim)
	replacement := demandHolder("chosen", 2, 7, now)
	r.mu.Lock()
	r.byID[replacement.id] = replacement
	replacement.reservation = reservation{LeaseConn: 7}
	r.genSeq++
	replacement.gen = r.genSeq
	r.mu.Unlock()

	if replacement.gen == v.notice.Gen {
		t.Fatal("the replacement reused the generation of the session it replaced — a slow " +
			"wake would end a session that was never selected")
	}
	// ASSERTED AT THE RESOLUTION, NOT THROUGH THE TEARDOWN. What is claimed
	// here is that the notice does not RESOLVE to the replacement; driving it
	// through the close as well meant the failing case ran into teardown and
	// panicked, and a panic says nothing about the claim.
	e := &Engine{sessions: r}
	if _, ok := e.demandTarget("chosen", v.notice.Gen); ok {
		t.Error("a notice about one session resolved to the session that replaced it, so a " +
			"slow wake would end somebody who was never selected")
	}
	if _, ok := e.demandTarget("chosen", replacement.gen); !ok {
		t.Error("a notice carrying the replacement's own generation did not resolve to it")
	}
	if replacement.get() != sessOpen {
		t.Error("the replacement session was torn down by a notice that was never about it")
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
	e.completeDemandReason(s, DemandDelivered)

	got := demandRecord(t, s).IdleMS
	if want := (30 * time.Minute).Milliseconds(); got != want {
		t.Errorf("idle_ms = %d, want the selection-time %d. Measuring the silence again when "+
			"the record is written means a client that is slow to accept its frame "+
			"inflates the justification for ending it", got, want)
	}
}

// demandRecord decodes the fields a demand close records.
//
// ONE DECODER, SO NO CELL GOES BACK TO MATCHING PROSE. The detail is data
// precisely so that nothing downstream has to regex an audit trail, and a test
// that asserts substrings is the first thing downstream.
func demandRecord(t *testing.T, s *session) struct {
	ReclaimState   string         `json:"reclaim_state"`
	IdleMS         int64          `json:"idle_ms"`
	ClientDelivery DemandDelivery `json:"client_delivery"`
} {
	t.Helper()
	s.mu.Lock()
	why := s.closeWhy
	s.mu.Unlock()

	var got struct {
		ReclaimState   string         `json:"reclaim_state"`
		IdleMS         int64          `json:"idle_ms"`
		ClientDelivery DemandDelivery `json:"client_delivery"`
	}
	if !strings.HasPrefix(why, ReasonDemandReclaimed+" ") {
		t.Fatalf("close reason = %q, want the stable identity first then its fields", why)
	}
	payload := strings.TrimPrefix(why, ReasonDemandReclaimed+" ")
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("the detail is not decodable data (%v): %q", err, payload)
	}
	return got
}

// A HOLDER OF OBJECTS IS ENDED LIKE ANY OTHER, AND THE RECORD SAYS WHICH IT WAS.
//
// AN EARLIER DESIGN ALLOWED AN IDLE HOLDER WITH AN EMPTY BACKEND TO HAVE THAT
// BACKEND DETACHED WHILE ITS SESSION CARRIED ON. That is not reclamation here,
// and the reason is in this package: the scheduled unit is the wire LEASE,
// taken in admitWithLeaseOrWait and held for the session's whole lifetime, so
// detaching a backend frees no lease and the request waiting for one is no
// better off. Both kinds therefore terminate.
//
// The distinction survives in the record rather than in the behaviour, and it
// earns its place: a target whose reclamations are mostly holders of objects is
// one where clients are leaving statements open, which is a different
// operational story — and a different fix — from idle connections nobody closed.
func TestDemandReclaim_AHolderOfObjectsIsEndedAndTheRecordSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stock func(*session)
		wants string
	}{
		{"an empty backend", func(*session) {}, "clean"},
		{
			name: "a prepared statement still on the backend",
			stock: func(s *session) {
				s.ext = &extObjects{
					statements: map[string]*extStatement{"s1": {}},
					portals:    map[string]*extPortal{},
				}
			},
			wants: "holds_objects",
		},
		{
			name: "a portal still on the backend",
			stock: func(s *session) {
				s.ext = &extObjects{
					statements: map[string]*extStatement{},
					portals:    map[string]*extPortal{"p1": {}},
				}
			},
			wants: "holds_objects",
		},
		{
			name: "a queued Close the target may not have acted on",
			stock: func(s *session) {
				s.ext = &extObjects{
					statements:    map[string]*extStatement{},
					portals:       map[string]*extPortal{},
					pendingCloses: []objectRef{{}},
				}
			},
			wants: "holds_objects",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
			tc.stock(s)
			r := demandRegistry(t, s)
			e := &Engine{sessions: r, now: func() time.Time { return now }}

			v, ok := r.reserveDemandVictim(7, now)
			if !ok {
				t.Fatal("a holder was not reclaimed; holding objects must not make a session " +
					"safe from reclamation, only differently recorded")
			}
			if got := v.notice.HeldObjects; got != (tc.wants == "holds_objects") {
				t.Errorf("notice.HeldObjects = %v for %s", got, tc.name)
			}

			e.completeDemandReason(s, DemandDelivered)
			if got := demandRecord(t, s).ReclaimState; got != tc.wants {
				t.Errorf("reclaim_state = %q, want %q", got, tc.wants)
			}
		})
	}
}

// ONE ENDING LEAVES ONE RECORD, AS DATA, WHETHER OR NOT THE CLIENT HEARD IT.
//
// The flush-failed path is the one worth pinning. A client that has stopped
// reading still has to be ended -- holding its lease would punish everybody
// waiting to protect somebody who is not listening -- and it is exactly the
// path where a second record, or none, would be easiest to miss.
//
// THE DETAIL IS DECODED, NOT MATCHED. An earlier version wrote an English
// sentence and this cell asserted substrings of it, which meant the record read
// well and could not be queried: anything downstream wanting the idle time or
// the delivery outcome would have had to regex an audit trail. Asserting
// against the decoded fields is what keeps it data.
func TestDemandReclaim_OneRecordCarriesTheFactsAsData(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivery  DemandDelivery
		held      bool
		wantState string
	}{
		{"the client was told", DemandDelivered, false, "clean"},
		{"the client had already gone", DemandFlushFailed, false, "clean"},
		{"nothing could be sent at all", DemandNotAttempted, false, "clean"},
		{"a holder of objects", DemandDelivered, true, "holds_objects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := demandHolder("holder", 1, 7, now.Add(-45*time.Minute))
			if tc.held {
				s.ext = &extObjects{
					statements: map[string]*extStatement{"s1": {}},
					portals:    map[string]*extPortal{},
				}
			}
			r := demandRegistry(t, s)
			e := &Engine{sessions: r, now: func() time.Time { return now }}

			if _, ok := r.reserveDemandVictim(7, now); !ok {
				t.Fatal("the holder was not reserved")
			}
			e.completeDemandReason(s, tc.delivery)

			s.mu.Lock()
			why := s.closeWhy
			s.mu.Unlock()

			// EXACTLY ONE stable reason, then fields -- not one reason plus a
			// second record elsewhere saying the same thing.
			if n := strings.Count(why, ReasonDemandReclaimed); n != 1 {
				t.Errorf("the close reason names the reclamation %d times, want 1: %q", n, why)
			}
			if !strings.HasPrefix(why, ReasonDemandReclaimed+" ") {
				t.Fatalf("close reason = %q, want the stable identity first then its fields", why)
			}

			var got struct {
				ReclaimState   string         `json:"reclaim_state"`
				IdleMS         int64          `json:"idle_ms"`
				ClientDelivery DemandDelivery `json:"client_delivery"`
			}
			payload := strings.TrimPrefix(why, ReasonDemandReclaimed+" ")
			if err := json.Unmarshal([]byte(payload), &got); err != nil {
				t.Fatalf("the detail is not decodable data (%v): %q — anything wanting these "+
					"facts would have to regex an audit trail", err, payload)
			}
			if got.ReclaimState != tc.wantState {
				t.Errorf("reclaim_state = %q, want %q", got.ReclaimState, tc.wantState)
			}
			if got.ClientDelivery != tc.delivery {
				t.Errorf("client_delivery = %q, want %q", got.ClientDelivery, tc.delivery)
			}
			if got.IdleMS != (45 * time.Minute).Milliseconds() {
				t.Errorf("idle_ms = %d, want the selection-time %d",
					got.IdleMS, (45 * time.Minute).Milliseconds())
			}
		})
	}
}

// THE ENGINE ACTUALLY WIRES DEMAND TO THE SCHEDULER.
//
// THIS CELL EXISTS BECAUSE ITS CONTROL CAME BACK GREEN. Setting the engine's
// reclaimer to nil broke nothing any cell could see: every part of demand
// reclamation was proved in isolation, and nothing asserted that the scheduler
// can reach it at all. A feature can be entirely correct and entirely
// unreachable, which is how the first version of the admission queue shipped
// as dead code — the same mistake, one layer up.
func TestDemandReclaim_TheEngineWiresItToTheScheduler(t *testing.T) {
	e := New(nil, nil)
	if e.sessions == nil {
		t.Fatal("the engine has no session registry")
	}
	if e.sessions.onDemand == nil {
		t.Fatal("the engine did not install its reclaimer on the registry, so the scheduler " +
			"can never ask for a lease back and every part of demand reclamation is correct " +
			"and unreachable")
	}
}

// THE PREDICATE AND THE RESERVATION HAPPEN UNDER ONE HOLD OF THE LOCK.
//
// THIS CELL EXISTS BECAUSE ITS CONTROL CAME BACK GREEN TOO. Releasing and
// retaking the session's mutex between judging it idle and claiming it is
// invisible unless something actually runs in the gap — so the control proved
// nothing, and the guarantee rested on reading the code. The hook below is
// something that runs in the gap: it makes the session busy, exactly as a
// client's arriving statement would, and a reservation taken anyway is one
// taken against a session that is no longer idle.
func TestDemandReclaim_NothingCanSlipBetweenJudgingAndClaiming(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
	r := demandRegistry(t, s)

	// Fires while the registry holds this session's lock, if it ever lets go.
	r.hookDemandJudged = func() {
		s.busy = true // no lock taken: only reachable if the hold was released
	}

	v, ok := r.reserveDemandVictim(7, now)
	if ok {
		t.Errorf("reserved %q although a statement started between the check and the claim; "+
			"the session would be terminated after becoming active, which is the one thing "+
			"the predicate exists to prevent", v.s.id)
	}
	if s.get() != sessOpen {
		t.Error("the holder was left reserved for teardown despite not being reserved")
	}
}
