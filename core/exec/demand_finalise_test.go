package exec

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// ONE RESERVATION, ONE FINALISATION, HOWEVER MANY CALLERS PRESENT IT.
//
// The notice carries an id and a generation, and both stay valid until the
// session leaves the registry. So two callers presenting the same pair before
// removal BOTH matched, both were told they owned the teardown, and both went
// on to perform one -- one ending written twice into the trail and one lease
// released twice, by a return value whose entire job was to say that had not
// happened. A name can be presented twice; a claim can only be consumed once.
//
// THE BARRIER IS THE POINT. Calling twice in sequence would pass against a
// check-then-claim in two holds, which is the same race in a smaller window.
// These callers are released together.
func TestDemandFinalisation_ManyCallersPresentingOneNoticeYieldOneOwner(t *testing.T) {
	now := time.Now()
	s := demandHolder("holder", 1, 7, now.Add(-30*time.Minute))
	r := demandRegistry(t, s)
	e := &Engine{sessions: r, now: func() time.Time { return now }}

	if _, ok := r.reserveDemandVictim(7, now); !ok {
		t.Fatal("the holder was not reserved, so there is no finalisation to contend for")
	}

	const callers = 8
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		owned int
	)
	start.Add(1)
	for range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if e.claimDemandFinalisation(s, DemandDelivered) {
				mu.Lock()
				owned++
				mu.Unlock()
			}
		}()
	}
	start.Done()
	done.Wait()

	if owned != 1 {
		t.Errorf("%d of %d callers were told they owned the teardown; every one of them "+
			"would tear the session down, audit the ending and release the lease",
			owned, callers)
	}

	// AND THE RECORD WAS WRITTEN ONCE. A second finalisation would append its
	// own outcome to a reason that already carried one.
	s.mu.Lock()
	why := s.closeWhy
	s.mu.Unlock()
	if n := strings.Count(why, "reclaim_state"); n != 1 {
		t.Errorf("the close reason carries %d outcomes, want 1: %q", n, why)
	}
}

// A FINALISATION IS REFUSED WHERE IT WOULD BE WRITING OVER SOMEBODY ELSE.
//
// The claim is not the only thing checked, because the claim alone cannot tell
// these apart: a session that was never reserved has no claim, but a session
// ended by an operator or by its own client while a stale notice was in flight
// is closing for a reason of its own, and a reclamation written over it would
// replace a true record with a false one.
func TestDemandFinalisation_ItIsRefusedAgainstAnEndingItDoesNotOwn(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		spoil func(s *session)
	}{
		{
			name:  "nobody reserved it",
			spoil: func(s *session) {},
		},
		{
			name: "it is closing for a reason of its own",
			spoil: func(s *session) {
				s.mu.Lock()
				s.demandFinal = true
				s.beginCloseLocked("", "operator-requested")
				s.mu.Unlock()
			},
		},
		{
			name: "it is still open",
			spoil: func(s *session) {
				s.mu.Lock()
				s.demandFinal = true
				s.mu.Unlock()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := demandHolder("holder", 1, 7, now.Add(-time.Hour))
			r := demandRegistry(t, s)
			e := &Engine{sessions: r, now: func() time.Time { return now }}
			tc.spoil(s)

			if e.claimDemandFinalisation(s, DemandDelivered) {
				t.Error("a finalisation was accepted although this reclamation does not own " +
					"the ending; its outcome would be written over a record that is true")
			}
		})
	}
}
