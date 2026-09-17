package pressure

import (
	"testing"
	"time"
)

func occ(name, subject string, value, capacity int) Reading {
	return Reading{
		Signal: Signal{ID: ID{Name: name, Subject: subject}, Class: Capacity, Kind: Occupancy},
		Value:  value, Cap: capacity,
	}
}

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

// A FIGURE THAT HOVERS PRODUCES ONE RAISE AND ONE CLEAR, NOT A STORM.
//
// THIS IS WHAT THE TWO THRESHOLDS ARE FOR, and it is the difference between a
// signal an operator reads and a signal an operator filters. A single threshold
// on a figure sitting near it emits on every tick; people build a rule to hide
// it, and then the one crossing that mattered is hidden with the rest.
func TestPressure_AHoveringFigureRaisesOnceAndClearsOnce(t *testing.T) {
	tr := NewTracker(fixed(time.Now()))

	// Cap 10: enters at 8, clears at 7.
	raises, clears := 0, 0
	for _, v := range []int{7, 8, 9, 8, 9, 8, 7, 8, 7, 6, 7, 6} {
		for _, e := range tr.Observe([]Reading{occ("sessions.global", "", v, 10)}) {
			if e.Entered {
				raises++
			} else {
				clears++
			}
		}
	}
	if raises != 2 || clears != 2 {
		t.Errorf("raises=%d clears=%d over a figure crossing twice; every enter needs "+
			"exactly one clear and a figure between the thresholds must change nothing",
			raises, clears)
	}
}

// THE BOUNDARY LANDS ON THE STATED INTEGER, AT EVERY CAP AND AT BOTH ENDS.
//
// Each row states where the signal must raise and where it must clear, and the
// cell asserts one below and one above each as well, so a rule that drifted by
// one is caught rather than merely looking plausible. The reported threshold is
// asserted too: an operator reads it to know how much room is left, and a
// figure that is right while the number beside it is wrong is worse than
// silence.
//
// WHAT THIS CELL DOES NOT PROVE is that integer arithmetic is load-bearing. I
// substituted the float form and this cell still passed at every cap here,
// which is why the comment on the constants says the integer form needs no
// rounding argument rather than claiming the float one misbehaves.
func TestPressure_TheOccupancyBoundaryIsExact(t *testing.T) {
	for _, tc := range []struct {
		capacity, enterAt, clearAt int
	}{
		{capacity: 10, enterAt: 8, clearAt: 7},
		{capacity: 5, enterAt: 4, clearAt: 3},
		{capacity: 3, enterAt: 3, clearAt: 2},
		{capacity: 1, enterAt: 1, clearAt: 0},
	} {
		tr := NewTracker(fixed(time.Now()))
		if ev := tr.Observe([]Reading{occ("leases.target", "7", tc.enterAt-1, tc.capacity)}); len(ev) != 0 {
			t.Errorf("cap %d raised at %d, one below the stated entry %d",
				tc.capacity, tc.enterAt-1, tc.enterAt)
		}
		ev := tr.Observe([]Reading{occ("leases.target", "7", tc.enterAt, tc.capacity)})
		if len(ev) != 1 || !ev[0].Entered {
			t.Fatalf("cap %d did not raise at %d", tc.capacity, tc.enterAt)
		}
		if ev[0].Threshold != tc.enterAt {
			t.Errorf("cap %d reported threshold %d, want %d — an operator reads this to "+
				"know how much room is left", tc.capacity, ev[0].Threshold, tc.enterAt)
		}
		if ev := tr.Observe([]Reading{occ("leases.target", "7", tc.clearAt+1, tc.capacity)}); len(ev) != 0 {
			t.Errorf("cap %d cleared at %d, one above the stated clear %d",
				tc.capacity, tc.clearAt+1, tc.clearAt)
		}
		ev = tr.Observe([]Reading{occ("leases.target", "7", tc.clearAt, tc.capacity)})
		if len(ev) != 1 || ev[0].Entered {
			t.Fatalf("cap %d did not clear at %d", tc.capacity, tc.clearAt)
		}
		// THE CLEAR CARRIES ITS OWN THRESHOLD, and this assertion was missing
		// while the comment above promised it -- so returning a flat zero here
		// survived every cell. The number beside the figure is the one an
		// operator uses to know how much room came back.
		if ev[0].Threshold != tc.clearAt {
			t.Errorf("cap %d reported clear threshold %d, want %d",
				tc.capacity, ev[0].Threshold, tc.clearAt)
		}
	}
}

// THE TWO CLASSES ARE INDEPENDENT, AND THIS IS THE INCIDENT'S LESSON IN THE
// OPERATOR'S VIEW.
//
// Admission was corrected so that running out of capacity is never charged as a
// credential failure. If the view then reported a throttled source as capacity
// pressure, the same conflation would return one layer up: somebody would read
// a credential signal as the pool being full and resize something that was
// never the problem. Neither class may suppress or clear the other.
func TestPressure_TheTwoClassesRaiseAndClearIndependently(t *testing.T) {
	tr := NewTracker(fixed(time.Now()))
	cap9 := occ("leases.target", "9", 9, 10)
	throttled := Reading{
		Signal: Signal{ID: ID{Name: "sources.throttled", Subject: "10.0.0.5"},
			Class: Credential, Kind: Count},
		Value: 1,
	}

	ev := tr.Observe([]Reading{cap9, throttled})
	if len(ev) != 2 {
		t.Fatalf("got %d events for two signals crossing together, want 2", len(ev))
	}
	byClass := map[Class]Event{}
	for _, e := range ev {
		byClass[e.Class] = e
	}
	if _, ok := byClass[Capacity]; !ok {
		t.Error("the lease signal did not raise as capacity")
	}
	if e, ok := byClass[Credential]; !ok {
		t.Error("the throttled source did not raise as credential — reporting it as " +
			"capacity would tell an operator to resize a pool over somebody guessing passwords")
	} else if e.Class == Capacity {
		t.Error("a throttled source was classed as capacity")
	}

	// The capacity signal clears; the credential one must be untouched.
	ev = tr.Observe([]Reading{occ("leases.target", "9", 7, 10), throttled})
	if len(ev) != 1 || ev[0].Class != Capacity || ev[0].Entered {
		t.Fatalf("clearing capacity produced %v, want exactly one capacity clear", ev)
	}
	if !tr.Raised(ID{Name: "sources.throttled", Subject: "10.0.0.5"}) {
		t.Error("clearing the capacity signal also cleared the credential one; the two " +
			"classes must raise and clear on their own schedules")
	}
}

// THE INCIDENT WOULD HAVE SAID SOMETHING, AND IT WOULD NOT HAVE NEEDED A CHARGE
// TO SAY IT.
//
// This is the cell that decides whether the scope did its job. The reconstruction
// is the shape of the original failure: a target's leases at their budget and
// capacity denials accumulating. It must raise capacity pressure naming the
// signal, the subject, the value and the threshold — and it must do so WITHOUT
// anything being charged to the source and WITHOUT anybody being throttled,
// because the fix that came before this removed exactly that behaviour. A cell
// that needed the charge would pass only while the bug was present.
func TestPressure_TheIncidentRaisesCapacityWithoutAnyCharge(t *testing.T) {
	now := time.Now()
	tr := NewTracker(fixed(now))

	var denials rateWindow
	for range denialsEnter {
		denials.add(now)
	}

	ev := tr.Observe([]Reading{
		occ("leases.target", "7", 10, 10),
		{Signal: Signal{ID: ID{Name: "denials.rate", Subject: "capacity"},
			Class: Capacity, Kind: Rate}, Value: denials.total(now)},
	})
	if len(ev) != 2 {
		t.Fatalf("the incident shape produced %d events, want 2 (%v)", len(ev), ev)
	}
	for _, e := range ev {
		if e.Class != Capacity {
			t.Errorf("%s raised as %s, want capacity", e.ID, e.Class)
		}
		if !e.Entered {
			t.Errorf("%s did not enter", e.ID)
		}
		if e.Value < e.Threshold {
			t.Errorf("%s raised with value %d below its threshold %d",
				e.ID, e.Value, e.Threshold)
		}
		if e.ID.Subject == "" {
			t.Errorf("%s named no subject; an operator needs to know WHICH target or reason", e.ID)
		}
	}
}

// A RAISED SIGNAL WHOSE SUBJECT DISAPPEARS STILL GETS ITS CLEAR.
//
// A per-user or per-source signal outlives its subject the moment that user
// disconnects or that throttle expires. Without this the view keeps showing a
// source nobody has seen for an hour, and the operator learns the view lies.
func TestPressure_ASignalWhoseSubjectIsGoneIsCleared(t *testing.T) {
	tr := NewTracker(fixed(time.Now()))
	if ev := tr.Observe([]Reading{occ("sessions.user", "alice", 9, 10)}); len(ev) != 1 {
		t.Fatalf("alice's signal did not raise: %v", ev)
	}
	ev := tr.Observe(nil) // alice has gone
	if len(ev) != 1 || ev[0].Entered {
		t.Fatalf("got %v, want one clear for the signal whose subject vanished", ev)
	}
	if tr.RaisedCount() != 0 {
		t.Errorf("%d signals still raised after their subjects went away", tr.RaisedCount())
	}
}
