package exec

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// A BEGIN ADMITTED IN THE DECISION WINDOW MUST NOT BE LOST.
//
// The defect this cell exists for is a check-then-act race, not a data race,
// so `-race` is blind to it and a sequential BEGIN-then-shutdown cell cannot
// reach it. The old handler read a zero count, audited, and only then closed
// the server; a BEGIN admitted in that window was torn down by the drain.
//
// So the window is DRIVEN rather than hoped for: the opening is parked after
// it takes admission, the shutdown decision runs while it is parked, and the
// invariant is asserted over the pair.
//
// THE INVARIANT IS THE PAIR, not either outcome alone. Both orderings are
// legal -- the opening may win admission and the shutdown then refuse, or the
// shutdown may close admission first and the opening be refused having started
// nothing. What must never happen is BOTH: a transaction that began AND a
// shutdown that was accepted.
func TestShutdownDecision_AnAdmittedBeginIsNeverLost(t *testing.T) {
	t.Parallel()

	for i := 0; i < 200; i++ {
		r := newSessionRegistry(64, 64)
		s := &session{id: "racer", userID: 1, connID: 1}
		r.mu.Lock()
		r.byID[s.id] = s
		r.mu.Unlock()

		parked := make(chan struct{})
		proceed := make(chan struct{})

		var (
			wg        sync.WaitGroup
			beganOK   bool
			shutdownN int
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := r.enterTxStart()
			if err != nil {
				return // refused admission: the opening never started
			}
			close(parked)
			<-proceed
			// Publish the phase exactly as beginTx does, under the session's
			// own lock, while still holding admission.
			s.mu.Lock()
			s.txPhase = txActive
			s.mu.Unlock()
			beganOK = true
			release()
		}()

		// Let the opening take admission when it is going to, then decide.
		select {
		case <-parked:
		case <-time.After(time.Second):
			t.Fatal("the opening never reached the admission point")
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			// closeTxAdmission must WAIT for the parked opening to release,
			// which is what makes the decision atomic with the count.
			shutdownN = r.closeTxAdmission()
		}()

		// Unpark: the decision is now racing a publish that already holds
		// admission. It must observe it.
		time.Sleep(time.Millisecond)
		close(proceed)
		wg.Wait()

		shutdownAccepted := shutdownN == 0
		if beganOK && shutdownAccepted {
			t.Fatalf("iteration %d: a transaction BEGAN and the shutdown was ACCEPTED; "+
				"the drain would now roll that transaction back", i)
		}
		if !beganOK && !shutdownAccepted {
			t.Fatalf("iteration %d: the opening was refused AND the shutdown refused "+
				"(count=%d); nothing holds a transaction, so the shutdown had no "+
				"reason to refuse", i, shutdownN)
		}
	}
}

// AND ONCE ADMISSION IS CLOSED, NOTHING NEW BEGINS -- with the reopen path
// asserted too, or a committed-then-abandoned shutdown would leave the daemon
// refusing transactions it is never going to end.
func TestTxAdmission_ClosesAndReopens(t *testing.T) {
	t.Parallel()
	r := newSessionRegistry(64, 64)

	// POSITIVE CONTROL: admission is open to begin with, or every assertion
	// below would hold for a gate that refused everything always.
	release, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("admission refused on a fresh registry: %v", err)
	}
	release()

	if n := r.closeTxAdmission(); n != 0 {
		t.Fatalf("closeTxAdmission reported %d open transactions on an empty registry", n)
	}
	if _, err := r.enterTxStart(); !errors.Is(err, ErrServerStopping) {
		t.Errorf("a transaction was admitted after the shutdown committed: err=%v", err)
	}

	r.reopenTxAdmission()
	release2, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("admission still refused after reopen: %v", err)
	}
	release2()
}

// A shutdown that finds an open transaction must NOT close admission -- it
// refused, so nothing about the daemon may change.
func TestTxAdmission_ARefusedShutdownLeavesAdmissionOpen(t *testing.T) {
	t.Parallel()
	r := newSessionRegistry(64, 64)
	s := &session{id: "holder", userID: 1, connID: 1}
	s.txPhase = txActive
	r.mu.Lock()
	r.byID[s.id] = s
	r.mu.Unlock()

	if n := r.closeTxAdmission(); n != 1 {
		t.Fatalf("closeTxAdmission reported %d, want 1", n)
	}
	release, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("a REFUSED shutdown closed admission anyway: %v", err)
	}
	release()
}
