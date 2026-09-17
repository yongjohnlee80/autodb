package exec

import "testing"

// THE SNAPSHOT REPORTS WHAT THE REGISTRY HOLDS, AND HANDS OUT COPIES.
//
// Two separate claims. The figures must match the caps that are actually being
// enforced, or the view describes a different instance than the one refusing
// people. And the maps must be copies: handing out the live ones lets a reader
// range over them while an admission writes, which is a data race the caller
// cannot see and cannot fix.
func TestCapacitySnapshot_ItReadsTheLiveCapsAndCopiesTheMaps(t *testing.T) {
	r := newSessionRegistry(4, 10)
	r.leaseCap = 2
	e := &Engine{sessions: r}

	s := &session{id: "holder", userID: 7, connID: 3}
	if err := r.admitWithLease(s, 3, 0); err != nil {
		t.Fatal(err)
	}

	snap := e.CapacitySnapshot()
	if snap.SessionCap != 10 || snap.PerUserCap != 4 || snap.LeaseCap != 2 {
		t.Errorf("caps read as global=%d peruser=%d lease=%d, want 10/4/2 — the view must "+
			"describe the same instance that is refusing people",
			snap.SessionCap, snap.PerUserCap, snap.LeaseCap)
	}
	if snap.Sessions != 1 || snap.PerUser[7] != 1 || snap.Leases[3] != 1 {
		t.Errorf("occupancy read as sessions=%d user7=%d target3=%d, want 1/1/1",
			snap.Sessions, snap.PerUser[7], snap.Leases[3])
	}

	// MUTATING WHAT WE WERE GIVEN MUST NOT REACH THE REGISTRY.
	snap.PerUser[7] = 999
	snap.Leases[3] = 999
	again := e.CapacitySnapshot()
	if again.PerUser[7] != 1 || again.Leases[3] != 1 {
		t.Error("the snapshot handed out the registry's own maps; a reader ranging over " +
			"them while an admission writes is a race the caller cannot see")
	}
}

// AN ENGINE WITH NO REGISTRY ANSWERS RATHER THAN PANICS.
//
// The pressure tick runs on a timer. A nil registry during start-up or teardown
// must give an empty reading, not take the process down — observability may
// never be the thing that ends the service it is observing.
func TestCapacitySnapshot_NoRegistryIsAnEmptyAnswer(t *testing.T) {
	var e Engine
	if got := e.CapacitySnapshot(); got.Sessions != 0 || got.SessionCap != 0 {
		t.Errorf("got %+v for an engine with no registry, want the zero snapshot", got)
	}
}
