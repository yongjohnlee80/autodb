package exec

import (
	"context"
	"testing"
	"time"
)

// A BUSY WIRE DOES NOT RESET THE DEPENDENCY CLOCK.
//
// THIS IS THE WHOLE REASON R7 IS NOT A WIRE-IDLE TIMER. A client that keeps
// chatting but never executes what it prepared pins a backend indefinitely, and
// an idle timer never notices because the wire is not idle. If ordinary traffic
// advanced this clock, R7 would be unreachable in exactly the case it exists
// for, and would look correct while never firing.
func TestR7_OrdinaryTrafficDoesNotAdvanceTheDependencyClock(t *testing.T) {
	at := time.Unix(0, 0)
	o := newExtObjectsAt(func() time.Time { return at })

	// An object is prepared and then nothing touches it again.
	if err := o.putStatement(&extStatement{name: "s1"}); err != nil {
		t.Fatal(err)
	}
	born := at

	// Two hours of wire traffic that does not move the dependency: queued
	// frames, synthesised answers, refusals. None of it is progress.
	at = at.Add(2 * time.Hour)
	o.queueWire()
	o.queueSynth()
	o.queueRefusal(nil)

	if got := o.dependencyIdleFor(at); got < 2*time.Hour {
		t.Errorf("the dependency clock advanced to %s of idleness after two hours of "+
			"unrelated traffic; R7 would never fire for the client it exists to "+
			"catch — one that keeps the wire busy and never uses what it prepared", got)
	}
	if o.lastProgress != born {
		t.Errorf("lastProgress moved from %v to %v without the dependency moving",
			born, o.lastProgress)
	}
}

// AND ANYTHING THAT MOVES THE DEPENDENCY DOES RESET IT.
//
// The other half: a session genuinely using its objects must never be reclaimed
// as though it had abandoned them.
func TestR7_TouchingTheObjectsResetsTheClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(o *extObjects)
	}{
		{"a statement is parsed", func(o *extObjects) { _ = o.putStatement(&extStatement{name: "a"}) }},
		{"a portal is bound", func(o *extObjects) { _ = o.putPortal(&extPortal{name: "p"}) }},
		{"a portal is executed", func(o *extObjects) { o.queueExec() }},
		{"a statement is closed", func(o *extObjects) { o.dropStatement("a") }},
		{"a portal is closed", func(o *extObjects) { o.dropPortal("p") }},
		{"a close is confirmed", func(o *extObjects) { o.confirmClose(objectRef{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Unix(0, 0)
			o := newExtObjectsAt(func() time.Time { return at })
			at = at.Add(3 * time.Hour)
			tc.move(o)
			if got := o.dependencyIdleFor(at); got != 0 {
				t.Errorf("after %s the clock still reports %s of idleness; a session "+
					"using its objects would be reclaimed as though it had abandoned "+
					"them", tc.name, got)
			}
		})
	}
}

// A SESSION HOLDING NOTHING PINS NOTHING.
//
// R7's precondition is a non-empty store. Without this the rung would consider
// sessions that are holding no backend state at all, which the idle timeout
// already owns.
func TestR7_AnEmptyStoreIsNotACandidate(t *testing.T) {
	at := time.Unix(0, 0)
	o := newExtObjectsAt(func() time.Time { return at })
	if o.holdsAnything() {
		t.Error("a fresh store reports holding something")
	}
	if err := o.putStatement(&extStatement{name: "s"}); err != nil {
		t.Fatal(err)
	}
	if !o.holdsAnything() {
		t.Error("a store with a parsed statement reports holding nothing; R7 would skip " +
			"exactly the session that is pinning a backend")
	}

	// A CLOSE THAT THE TARGET HAS NOT CONFIRMED IS STILL HELD STATE, and this
	// case had no coverage: the cell above keeps a statement in the store, so
	// dropping pendingCloses from the check changed nothing and the control for
	// it came back GREEN. The name is free on our side while the TARGET may
	// still hold the object, which is precisely the state that pins a backend
	// without anything obvious in the maps.
	empty := newExtObjectsAt(func() time.Time { return at })
	empty.notePendingClose(objectRef{})
	if !empty.holdsAnything() {
		t.Error("a store whose only content is an unconfirmed close reports holding " +
			"nothing; the target may still hold that object, and R7 would skip the " +
			"session pinning it")
	}
}

// r7Session builds a session holding an object whose dependency has stalled.
func r7Session(t *testing.T, at time.Time, stalled time.Duration) (*Engine, *session) {
	t.Helper()
	e := New(nil, nil)
	t.Cleanup(func() { _ = e.Close() })
	s := &session{id: "holder", userID: 1, connID: 7}
	s.state.Store(int32(sessOpen))
	s.lastUsed = at // wire-active
	s.ext = newExtObjectsAt(func() time.Time { return at.Add(-stalled) })
	if err := s.ext.putStatement(&extStatement{name: "prepared-and-forgotten"}); err != nil {
		t.Fatal(err)
	}
	return e, s
}

// THE RUNG FIRES FOR A TALKING CLIENT THAT HAS STOPPED USING WHAT IT HOLDS.
//
// This is the whole point of the milestone: a backend pinned by an object
// nobody has touched, on a connection nobody would call idle.
func TestR7_AStalledDependencyOnALiveWireIsReclaimed(t *testing.T) {
	at := time.Unix(0, 0).Add(100 * time.Hour)
	e, s := r7Session(t, at, r7DependencyBound+time.Minute)

	if !e.r7Expired(s, at, 10*time.Minute) {
		t.Error("a session holding an object untouched for over the bound, on an active " +
			"wire, was not reclaimed; nothing else ever reclaims it, which is the hole " +
			"this rung exists to close")
	}
}

// AND IT DOES NOT FIRE FOR ANYBODY WHO WOULD BE HARMED.
//
// Each row is somebody with work in progress or with nothing to reclaim. A rung
// that took any of them would be worse than the leak it fixes.
func TestR7_ItSparesEverySessionThatWouldBeHarmed(t *testing.T) {
	at := time.Unix(0, 0).Add(100 * time.Hour)
	for _, tc := range []struct {
		name  string
		spoil func(s *session)
	}{
		{"a request is in flight", func(s *session) { s.busy = true }},
		{"a transaction is open", func(s *session) { s.tx = stubTxConn{} }},
		{"it holds nothing", func(s *session) { s.ext = newExtObjectsAt(func() time.Time { return at }) }},
		{"the wire has gone quiet", func(s *session) { s.lastUsed = at.Add(-time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := r7Session(t, at, r7DependencyBound+time.Minute)
			tc.spoil(s)
			if e.r7Expired(s, at, 10*time.Minute) {
				t.Errorf("the rung claimed a session where %s", tc.name)
			}
		})
	}
}

// THE SWEEP MUST NOT DEADLOCK AGAINST ITSELF.
//
// r7Expired computes the wire-idle age inline, under s.mu, instead of calling
// s.idleFor -- which takes that same mutex. A sync.Mutex is not reentrant, so
// the obvious tidy-up (reuse the helper that already does this) deadlocks the
// janitor: the daemon stops reaping sessions and never says why.
//
// THAT DEFECT WAS CAUGHT BY THE COMPILER REFUSING AN UNRELATED LINE, NOT BY A
// CELL, so until now nothing stopped it returning. The five cells above all
// call r7Expired directly and would hang to the package's ten-minute timeout
// panic, which names nothing.
//
// This one drives the PRODUCTION sweep and bounds the wait, so a re-entrant
// lock fails in a second with a message that says which lock and why.
func TestR7_TheSweepDoesNotDeadlockOnTheSessionMutex(t *testing.T) {
	at := time.Unix(0, 0).Add(100 * time.Hour)
	// Deliberately INSIDE the bound: the sweep must reach r7Expired, take the
	// lock and come back without closing anything. The lock path is the
	// subject here, not the verdict -- which the cells above already own.
	e, s := r7Session(t, at, r7DependencyBound-time.Minute)
	e.sessions.mu.Lock()
	e.sessions.byID[s.id] = s
	e.sessions.mu.Unlock()
	// The registry is the sweep's input, and it is also what engine shutdown
	// walks. This fixture has no pool behind it, so leaving it registered makes
	// the engine's own Close tear down a session that cannot be torn down.
	// Take it back out the moment the sweep has read it.
	defer func() {
		e.sessions.mu.Lock()
		delete(e.sessions.byID, s.id)
		e.sessions.mu.Unlock()
	}()

	done := make(chan int, 1)
	go func() { done <- e.reapIdleSessions(context.Background(), at) }()

	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("the sweep closed %d session(s); this fixture is one minute inside "+
				"R7's bound and on a live wire, so it should have survived both rungs", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the janitor sweep never returned: r7Expired holds s.mu and is calling " +
			"something that takes s.mu again (s.idleFor does exactly that). A sync.Mutex " +
			"is not reentrant, so in production this is a daemon that silently stops " +
			"reaping sessions -- no error, no log, just connections never given back")
	}
}

// A DEPENDENCY STILL INSIDE ITS BOUND IS LEFT ALONE.
func TestR7_ADependencyInsideItsBoundIsNotReclaimed(t *testing.T) {
	at := time.Unix(0, 0).Add(100 * time.Hour)
	e, s := r7Session(t, at, r7DependencyBound-time.Minute)
	if e.r7Expired(s, at, 10*time.Minute) {
		t.Error("a session one minute inside the bound was reclaimed; the bound is the " +
			"promise, and a rung that fires early breaks it for everybody legitimately " +
			"holding an object across a quiet period")
	}
}
