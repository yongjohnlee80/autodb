package pressure

import (
	"testing"
	"time"
)

// EVERY ROW COMES FROM A FIGURE THE CALLER ALREADY HAD.
//
// The scope's binding constraint is that the view adds no source of truth. This
// cell states it as an assertion rather than a promise: the same inputs must
// always give the same readings, and no reading may appear that no input
// accounts for.
func TestReadings_TheyAreDerivedAndNothingIsInvented(t *testing.T) {
	c := Caps{
		Sessions: 3, SessionCap: 10,
		PerUser: map[int64]int{7: 2, 9: 1}, PerUserCap: 4,
		Leases: map[int64]int{100: 5}, LeaseCap: 5,
	}
	first := Readings(c, 2, []string{"10.0.0.1"})
	second := Readings(c, 2, []string{"10.0.0.1"})
	if len(first) != len(second) {
		t.Fatalf("the same inputs produced %d and then %d readings", len(first), len(second))
	}
	// 1 global + 2 users + 1 target + 1 denial rate + 1 source.
	if len(first) != 6 {
		t.Errorf("got %d readings, want 6 — one per figure supplied, and nothing else", len(first))
	}
	for _, r := range first {
		if r.Kind == Occupancy && r.Cap <= 0 {
			t.Errorf("%s is an occupancy with cap %d; a fraction of no limit is not a "+
				"figure that can cross anything", r.ID, r.Cap)
		}
	}
}

// A CAP OF ZERO PRODUCES NO SIGNAL AT ALL.
//
// "No limit configured" must not become a row that can never raise. An operator
// who learns to skip a row learns to skip rows.
func TestReadings_UnconfiguredCapsProduceNoRows(t *testing.T) {
	got := Readings(Caps{Sessions: 99, PerUser: map[int64]int{1: 9}, Leases: map[int64]int{2: 9}}, 0, nil)
	for _, r := range got {
		if r.Kind == Occupancy {
			t.Errorf("%s was emitted with every cap unconfigured", r.ID)
		}
	}
	if len(got) != 1 || got[0].ID.Name != DenialsRate {
		t.Fatalf("got %v, want only the denial rate", got)
	}
}

// THE DENIAL RATE RAISES AND THEN CLEARS ON ITS OWN FIGURES.
//
// Ordinary regression coverage, and I want its limit stated: it does NOT prove
// that emitting the rate at zero is necessary. Omitting it produces an
// identical clear through the tracker's path for a vanished subject, which I
// confirmed by making that change and watching this cell pass unchanged. What
// it does prove is that a raised rate comes down again, which is the thing an
// operator is relying on when they stop watching.
func TestReadings_TheDenialRateSurvivesFallingToZero(t *testing.T) {
	tr := NewTracker(func() time.Time { return time.Unix(0, 0) })
	caps := Caps{SessionCap: 10, Sessions: 0}

	ev := tr.Observe(Readings(caps, denialsEnter, nil))
	if len(ev) != 1 || !ev[0].Entered || ev[0].ID.Name != DenialsRate {
		t.Fatalf("the denial rate did not raise: %v", ev)
	}
	ev = tr.Observe(Readings(caps, 0, nil))
	if len(ev) != 1 || ev[0].Entered || ev[0].ID.Name != DenialsRate {
		t.Fatalf("the denial rate did not clear on its own terms: %v", ev)
	}
	if ev[0].Value != 0 {
		t.Errorf("the clear reported value %d, want 0", ev[0].Value)
	}
}

// A THROTTLED SOURCE IS CREDENTIAL, AND IT LEAVES WHEN ITS WINDOW DOES.
//
// The class is the incident's lesson carried into operations: reported as
// capacity, it would send somebody to resize a pool over password guessing. And
// it must disappear when the throttle expires, or the view is describing an
// hour ago.
func TestReadings_AThrottledSourceIsCredentialAndThenGoes(t *testing.T) {
	tr := NewTracker(func() time.Time { return time.Unix(0, 0) })
	caps := Caps{SessionCap: 10}

	ev := tr.Observe(Readings(caps, 0, []string{"10.0.0.9"}))
	if len(ev) != 1 {
		t.Fatalf("got %v, want one event for the throttled source", ev)
	}
	if ev[0].Class != Credential {
		t.Errorf("the throttled source raised as %s; reporting it as capacity sends an "+
			"operator to resize a pool over somebody guessing passwords", ev[0].Class)
	}
	if ev[0].ID.Subject != "10.0.0.9" {
		t.Errorf("subject %q, want the source address — an operator needs to know WHICH",
			ev[0].ID.Subject)
	}

	ev = tr.Observe(Readings(caps, 0, nil)) // the window expired
	if len(ev) != 1 || ev[0].Entered {
		t.Fatalf("got %v, want one clear when the throttle expired", ev)
	}
	if tr.RaisedCount() != 0 {
		t.Error("the source is still raised after its window ended; a view that keeps it " +
			"teaches the operator that the view lies")
	}
}
