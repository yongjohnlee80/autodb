package pressure

import (
	"fmt"
	"testing"
	"time"
)

func caps(sessions, sessionCap, perUserCap, leaseCap int, users, leases map[int64]int) Caps {
	return Caps{Sessions: sessions, SessionCap: sessionCap,
		PerUser: users, PerUserCap: perUserCap, Leases: leases, LeaseCap: leaseCap}
}

// PER-USER ROWS ARE LOAD-BEARING, AND THEY ATTRIBUTE TO THE RIGHT USER.
//
// "Whose app is hungry" was the question nobody could answer during the
// incident, and it is the first one an operator asks. A row attributed to the
// wrong user is worse than no row: somebody goes and talks to the wrong team.
func TestSnapshot_PerUserRowsAttributeCorrectlyAndLeadWithTheHungriest(t *testing.T) {
	tr := NewTracker(func() time.Time { return time.Unix(0, 0) })
	s := tr.Assemble(ViewInput{Caps: caps(9, 100, 10, 10,
		map[int64]int{7: 2, 9: 8, 11: 5}, nil)})

	if len(s.PerUser) != 3 {
		t.Fatalf("got %d per-user rows, want 3: %+v", len(s.PerUser), s.PerUser)
	}
	if s.PerUser[0].Subject != "9" || s.PerUser[0].Value != 8 {
		t.Errorf("the first row is %+v, want user 9 with 8 — the surface is read "+
			"top-down and the hungriest is the answer somebody needs first", s.PerUser[0])
	}
	for _, r := range s.PerUser {
		want := map[string]int{"7": 2, "9": 8, "11": 5}[r.Subject]
		if r.Value != want {
			t.Errorf("user %s shows %d, want %d — a row attributed to the wrong user "+
				"sends somebody to talk to the wrong team", r.Subject, r.Value, want)
		}
	}
}

// THE VIEW AND THE JOURNAL AGREE ABOUT WHAT IS RAISED.
//
// If the view re-applied the threshold rule for itself it could disagree with
// the event stream about the same instant — the surface calm while the journal
// says it entered. One latch, read twice.
func TestSnapshot_RaisedComesFromTheSameLatchAsTheEvents(t *testing.T) {
	tr := NewTracker(func() time.Time { return time.Unix(0, 0) })
	in := ViewInput{Caps: caps(9, 10, 10, 10, nil, nil)}

	// Nothing observed yet, so nothing is raised even though the figure is over.
	if tr.Assemble(in).Sessions.Raised {
		t.Error("the view reported a raised signal the tracker had never raised; it is " +
			"deciding for itself and can contradict the journal")
	}

	tr.Observe(Readings(in.Caps, 0, nil))
	if !tr.Assemble(in).Sessions.Raised {
		t.Error("the tracker raised the signal and the view did not show it")
	}
}

// EVERY CAPPED SECTION SAYS WHAT IT LEFT OUT.
//
// I WROTE THIS ONE WRONG FIRST. The per-user and lease remainders were
// discarded — `_, _ =` — directly beneath a comment saying omitted totals are
// rendered. That is the same defect as a docstring promising an assertion the
// cell does not make, and it would have shipped a surface telling an operator
// sixteen users were hungry when four hundred were.
func TestSnapshot_TruncatedSectionsCarryTheirRemainder(t *testing.T) {
	tr := NewTracker(func() time.Time { return time.Unix(0, 0) })
	users := map[int64]int{}
	leases := map[int64]int{}
	for i := range 40 {
		users[int64(i)] = i
		leases[int64(i)] = i
	}
	var throttled []ThrottledRow
	for i := range 40 {
		throttled = append(throttled, ThrottledRow{
			Host: fmt.Sprintf("10.0.0.%d", i), Remaining: time.Duration(i) * time.Second})
	}

	s := tr.Assemble(ViewInput{Caps: caps(0, 100, 100, 100, users, leases),
		Throttled: throttled})

	for _, tc := range []struct {
		name  string
		shown int
		more  int
	}{
		{"per-user", len(s.PerUser), s.PerUserOmitted},
		{"leases", len(s.Leases), s.LeasesOmitted},
		{"throttled", len(s.Throttled), s.ThrottledOmitted},
	} {
		if tc.shown > maxRows {
			t.Errorf("%s showed %d rows, want at most %d", tc.name, tc.shown, maxRows)
		}
		if tc.shown+tc.more != 40 {
			t.Errorf("%s shows %d and reports %d more, which does not account for the 40 "+
				"that arrived; a truncated list without its true remainder tells the "+
				"operator the problem is smaller than it is", tc.name, tc.shown, tc.more)
		}
	}
}
