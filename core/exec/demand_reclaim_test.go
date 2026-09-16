package exec

import (
	"context"
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
	s.wake = func(DemandNotice) {}
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
		s.terminal.gen.Store(r.genSeq)
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
			name:  "it is not a front-door session",
			spoil: func(s *session) { s.wire = false },
			why:   "there is no client loop to frame it, and nobody to tell",
		},
		{
			name:  "nothing can reach its client",
			spoil: func(s *session) { s.wake = nil },
			why:   "ending a session nobody can explain to is worse than not reclaiming it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
			tc.spoil(s)
			r := demandRegistry(t, s)
			if _, ok := r.claimDemandVictim(7, now); ok {
				t.Fatalf("a holder was chosen while %s — %s", tc.name, tc.why)
			}
			if s.terminal.taken.Load() {
				t.Error("the claim was spent on a holder that was not chosen")
			}
		})
	}

	t.Run("idle, quiet and reachable", func(t *testing.T) {
		s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
		r := demandRegistry(t, s)
		v, ok := r.claimDemandVictim(7, now)
		if !ok {
			t.Fatal("an idle holder with no transaction, no request in flight and a live owner was not chosen")
		}
		if v.s != s {
			t.Error("a different session was chosen")
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

	v, ok := r.claimDemandVictim(7, now)
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
func TestDemandReclaim_TheClaimIsSingleUseUnderContention(t *testing.T) {
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
			if _, ok := r.claimDemandVictim(7, now); ok {
				wins.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	if n := wins.Load(); n != 1 {
		t.Errorf("%d of %d contenders claimed the same session, want exactly 1 — more than "+
			"one terminal owner means a client is framed twice", n, racers)
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
	v, ok := r.claimDemandVictim(7, now)
	if !ok {
		t.Fatal("no holder was chosen")
	}

	// The chosen session ends on its own, and another takes its place.
	r.remove(victim)
	replacement := demandHolder("chosen", 2, 7, now)
	r2 := demandRegistry(t, replacement)
	if replacement.terminal.valid(v.notice.Gen) {
		t.Fatal("a claim taken against one session is valid against its replacement — a slow " +
			"wake would end a session that was never selected")
	}
	e := &Engine{sessions: r2}
	if e.FinishDemandReclaim(context.Background(), "chosen", v.notice.Gen) {
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
