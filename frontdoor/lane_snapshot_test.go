package frontdoor

import (
	"testing"
	"time"
)

// "FOR HOW MUCH LONGER" IS NOT "THE OLDEST FAILURE PLUS THE WINDOW".
//
// THIS IS THE ROW THE ADR ASKS FOR AND THE OBVIOUS ANSWER IS WRONG. A source
// that failed more times than the limit does not stop being throttled when its
// FIRST failure ages out — the throttle lifts when the live count drops below
// the limit, so what matters is the (n-k+1)th oldest expiring. Reporting the
// first would tell an operator a source was about to be let in while it still
// had four failures inside the window, and somebody acting on that number finds
// nothing changed and stops believing the column.
func TestLaneSnapshot_RemainingIsWhenTheThrottleActuallyLifts(t *testing.T) {
	at := time.Unix(0, 0)
	clock := func() time.Time { return at }
	a := newAdmitter(16, 8, 3, 1<<20, clock)

	// Five failures, a limit of three, all inside one window so none has aged
	// out. Spacing them wider than AuthFailureWindow would let each expire
	// before the next arrived and the source would never be throttled at all —
	// which is how the first version of this cell failed on its own setup.
	const step = AuthFailureWindow / 10
	for i := range 5 {
		at = time.Unix(0, 0).Add(time.Duration(i) * step)
		a.noteFailure("10.0.0.1")
	}
	now := at
	snap := a.laneSnapshot(now)
	if len(snap.Throttled) != 1 {
		t.Fatalf("got %v, want the throttled source", snap.Throttled)
	}

	// Live count 5, limit 3, so the throttle lifts when the failure at index
	// 5-3=2 leaves the window — not when the first one does.
	want := time.Unix(0, 0).Add(2 * step).Add(AuthFailureWindow).Sub(now)
	if got := snap.Throttled[0].Remaining; got != want {
		naive := time.Unix(0, 0).Add(AuthFailureWindow).Sub(now)
		t.Errorf("remaining = %s, want %s (the naive oldest-plus-window answer would be "+
			"%s, and would promise the source was about to be let in while four "+
			"failures were still live)", got, want, naive)
	}
}

// THE LANE FIGURES AGREE WITH EACH OTHER, BECAUSE THEY ARE READ TOGETHER.
//
// Read separately, an occupancy can come from before an accept and its cap from
// after a reconfigure. A view showing a pre-auth count above its own cap reads
// as the instrument being broken rather than as a sampling artefact, and an
// operator who sees that once discounts the whole surface.
func TestLaneSnapshot_TheOccupanciesAreReadAgainstTheirOwnCaps(t *testing.T) {
	at := time.Unix(0, 0)
	a := newAdmitter(16, 8, 3, 1<<20, func() time.Time { return at })

	snap := a.laneSnapshot(at)
	if snap.MaxConns != 16 || snap.MaxPreAuth != 8 {
		t.Errorf("caps read as conns=%d preauth=%d, want 16/8", snap.MaxConns, snap.MaxPreAuth)
	}
	if snap.Conns > snap.MaxConns || snap.PreAuth > snap.MaxPreAuth {
		t.Errorf("an occupancy exceeded its own cap (%d/%d, %d/%d)",
			snap.Conns, snap.MaxConns, snap.PreAuth, snap.MaxPreAuth)
	}
}

// A SOURCE THAT IS NO LONGER THROTTLED IS NOT LISTED.
//
// The column answers "who is being held out right now". A source whose window
// has passed belongs to the past, and nothing else would ever remove it if it
// never dials again.
func TestLaneSnapshot_ExpiredThrottlesLeaveTheList(t *testing.T) {
	at := time.Unix(0, 0)
	a := newAdmitter(16, 8, 3, 1<<20, func() time.Time { return at })
	for range 3 {
		a.noteFailure("10.0.0.7")
	}
	if len(a.laneSnapshot(at).Throttled) != 1 {
		t.Fatal("the source was not throttled at all")
	}
	later := at.Add(AuthFailureWindow + time.Second)
	if got := a.laneSnapshot(later).Throttled; len(got) != 0 {
		t.Errorf("listed %v after the window passed; the column would describe the past", got)
	}
}
