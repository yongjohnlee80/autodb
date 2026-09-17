package exec

import (
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
}
