package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func regFor(t *testing.T, perUser, global, leaseCap int, residentCap int64) *sessionRegistry {
	t.Helper()
	r := newSessionRegistry(perUser, global)
	r.leaseCap = leaseCap
	r.residentCap = residentCap
	return r
}

func sess(id SessionID, userID, connID int64) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{id: id, userID: userID, connID: connID, ctx: ctx, cancel: cancel}
	s.state.Store(int32(sessOpen))
	return s
}

// Matrix row 2.7: FOUR members, acquired as one operation.
//
// The fourth — the session's fixed overhead charge — is the one a summary of
// this design dropped, and its absence is what recreates the check-then-
// reserve gap for memory while the other three are protected. Each is
// asserted to refuse on its own, because a reservation that only enforces
// three of four is a reservation with one unguarded resource.
func TestReservation_EachMemberRefusesOnItsOwn(t *testing.T) {
	t.Parallel()

	t.Run("the per-user session cap", func(t *testing.T) {
		t.Parallel()
		r := regFor(t, 2, 100, 100, 1<<30)
		for i := 0; i < 2; i++ {
			if err := r.admitWithLease(sess(SessionID(string(rune('a'+i))), 7, 1), 1, 16); err != nil {
				t.Fatalf("admit %d: %v", i, err)
			}
		}
		if err := r.admitWithLease(sess("over", 7, 1), 1, 16); !errors.Is(err, ErrSessionCapExceeded) {
			t.Errorf("err = %v, want ErrSessionCapExceeded", err)
		}
	})

	t.Run("the global session cap", func(t *testing.T) {
		t.Parallel()
		r := regFor(t, 100, 2, 100, 1<<30)
		for i := 0; i < 2; i++ {
			if err := r.admitWithLease(sess(SessionID(string(rune('a'+i))), int64(i), 1), 1, 16); err != nil {
				t.Fatalf("admit %d: %v", i, err)
			}
		}
		if err := r.admitWithLease(sess("over", 9, 1), 1, 16); !errors.Is(err, ErrSessionCapExceeded) {
			t.Errorf("err = %v, want ErrSessionCapExceeded", err)
		}
	})

	// The target lease. Its own identity, because the remedy differs: a
	// session cap says this user or server is full, this says the TARGET's
	// pool has no lease left.
	t.Run("the per-target wire lease", func(t *testing.T) {
		t.Parallel()
		r := regFor(t, 100, 100, 2, 1<<30)
		for i := 0; i < 2; i++ {
			if err := r.admitWithLease(sess(SessionID(string(rune('a'+i))), int64(i), 1), 1, 16); err != nil {
				t.Fatalf("admit %d: %v", i, err)
			}
		}
		err := r.admitWithLease(sess("over", 9, 1), 1, 16)
		if !errors.Is(err, ErrLeaseCapExceeded) {
			t.Fatalf("err = %v, want ErrLeaseCapExceeded", err)
		}
		// And it is PER TARGET: another connection still has leases.
		if err := r.admitWithLease(sess("other-conn", 9, 2), 2, 16); err != nil {
			t.Errorf("a different target was refused (%v); the lease cap is per-pool, not global", err)
		}
	})

	// The fourth member.
	t.Run("the fixed overhead charge", func(t *testing.T) {
		t.Parallel()
		r := regFor(t, 100, 100, 100, 100)
		if err := r.admitWithLease(sess("a", 1, 1), 1, 60); err != nil {
			t.Fatalf("first: %v", err)
		}
		err := r.admitWithLease(sess("b", 2, 1), 1, 60)
		if !errors.Is(err, ErrResidentBudgetExceeded) {
			t.Fatalf("err = %v, want ErrResidentBudgetExceeded — without this member the memory "+
				"budget is the one unguarded resource in a reservation that protects the "+
				"other three", err)
		}
	})
}

// Partial reservation is impossible: a refused connection holds NOTHING.
//
// This is the property that makes "acquired as one operation" mean something.
// A refusal that left a lease or a charge behind would be worse than the
// overrun it prevented — nothing is coming to release it, so the capacity is
// gone until the daemon restarts.
func TestReservation_ARefusalHoldsNothing(t *testing.T) {
	t.Parallel()
	r := regFor(t, 100, 100, 1, 1000)

	if err := r.admitWithLease(sess("holder", 1, 1), 1, 100); err != nil {
		t.Fatal(err)
	}
	leasesBefore, residentBefore := r.leaseCount(1), r.residentHeld()

	// Refused on the LEASE, after the session caps passed — so the earlier
	// members had already been checked when the refusal happened.
	if err := r.admitWithLease(sess("refused", 2, 1), 1, 100); !errors.Is(err, ErrLeaseCapExceeded) {
		t.Fatalf("expected a lease refusal, got %v", err)
	}
	if got := r.leaseCount(1); got != leasesBefore {
		t.Errorf("leases = %d after a refusal, want %d — the refused connection kept a lease",
			got, leasesBefore)
	}
	if got := r.residentHeld(); got != residentBefore {
		t.Errorf("resident = %d after a refusal, want %d — the refused connection kept its "+
			"memory charge, and nothing is coming to release it", got, residentBefore)
	}
	if _, found := r.lookup("refused", 2); found == nil {
		t.Error("a refused session is in the registry")
	}
}

// Everything is given back when the session leaves, under the same lock that
// removed it. A lease released separately would let a session leave the
// registry while still counted against its target — a leak that surfaces as a
// database refusing connections it has capacity for.
func TestReservation_RemovingASessionReturnsEverything(t *testing.T) {
	t.Parallel()
	r := regFor(t, 100, 100, 4, 1000)

	s := sess("a", 1, 7)
	if err := r.admitWithLease(s, 7, 250); err != nil {
		t.Fatal(err)
	}
	if r.leaseCount(7) != 1 || r.residentHeld() != 250 {
		t.Fatalf("after admit: leases=%d resident=%d", r.leaseCount(7), r.residentHeld())
	}
	r.remove(s)
	if got := r.leaseCount(7); got != 0 {
		t.Errorf("leases = %d after remove, want 0", got)
	}
	if got := r.residentHeld(); got != 0 {
		t.Errorf("resident = %d after remove, want 0", got)
	}
	// Removing twice must not credit capacity that was never held.
	r.remove(s)
	if got := r.residentHeld(); got != 0 {
		t.Errorf("resident = %d after a double remove, want 0 — a double release hands out "+
			"capacity that does not exist", got)
	}
}

// The caps hold when every member is contended at once. Per the concurrency
// convention the competing transitions are driven INSIDE the check→effect
// window rather than merely run alongside.
func TestReservation_HoldsUnderContention(t *testing.T) {
	t.Parallel()
	const cap = 4
	r := regFor(t, 100, 100, cap, 1<<30)

	var once sync.Once
	var wg sync.WaitGroup
	r.hookAfterAdmitCheck = func() {
		once.Do(func() {
			// Inside the window: every cap checked, nothing taken. The
			// competitors block on the lock, which is the property.
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_ = r.admitWithLease(sess(SessionID("c"+string(rune('a'+i))), int64(i), 3), 3, 8)
				}(i)
			}
		})
	}
	_ = r.admitWithLease(sess("first", 99, 3), 3, 8)
	wg.Wait()

	if got := r.leaseCount(3); got > cap {
		t.Fatalf("%d leases against a cap of %d: a cap observed free was not the cap acquired",
			got, cap)
	}
}
