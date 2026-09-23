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
			shutdownN, _ = r.closeTxAdmission()
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

	n, owner := r.closeTxAdmission()
	if n != 0 {
		t.Fatalf("closeTxAdmission reported %d open transactions on an empty registry", n)
	}
	if owner == 0 {
		t.Fatal("closing admission on an empty registry did not hand back ownership")
	}
	if _, err := r.enterTxStart(); !errors.Is(err, ErrServerStopping) {
		t.Errorf("a transaction was admitted after the shutdown committed: err=%v", err)
	}

	if !r.reopenTxAdmission(owner) {
		t.Fatal("the owner could not reopen its own admission")
	}
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

	n, owner := r.closeTxAdmission()
	if n != 1 {
		t.Fatalf("closeTxAdmission reported %d, want 1", n)
	}
	if owner != 0 {
		t.Errorf("a BLOCKED decision was handed ownership (token %d): it closed nothing", owner)
	}
	release, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("a REFUSED shutdown closed admission anyway: %v", err)
	}
	release()
}

// TWO DECIDERS, ONE COMMITTING AND ONE FAILING ITS AUDIT.
//
// The r1 defect: closeTxAdmission answered a bare zero both when it newly
// closed admission and when admission was ALREADY closed, and the reopen was
// unconditional. So two concurrent sys.shutdown handlers both read "clear to
// stop"; one committed, and the other's failed audit reopened admission
// underneath the first one's drain. A transaction admitted in that gap is the
// same loss as the original race, reached the long way round.
//
// The sequence is driven, not raced, because the defect is about OWNERSHIP
// rather than timing: A wins the decision, B arrives while it is held, B's
// audit fails, and admission must still be closed afterwards.
func TestShutdownDecision_ALoserCannotReopenTheWinnersAdmission(t *testing.T) {
	t.Parallel()
	r := newSessionRegistry(64, 64)

	aBlockers, aOwner := r.closeTxAdmission()
	if aBlockers != 0 || aOwner == 0 {
		t.Fatalf("A did not win the decision: blockers=%d owner=%d", aBlockers, aOwner)
	}

	// B arrives while A holds it. It must be told it does NOT own the decision,
	// or it would go on to commit a shutdown on top of A's.
	bBlockers, bOwner := r.closeTxAdmission()
	if bOwner != 0 {
		t.Fatalf("B was handed ownership (token %d) of a decision A already owns", bOwner)
	}
	if bBlockers != 0 {
		t.Fatalf("B reported %d blockers; nothing holds a transaction", bBlockers)
	}

	// B's audit fails, so B tries to undo a decision it never owned.
	if r.reopenTxAdmission(bOwner) {
		t.Error("B reopened admission it does not own: A's drain would now admit " +
			"transactions it is about to tear down")
	}
	// A stale/forged token must not work either.
	if r.reopenTxAdmission(aOwner + 1) {
		t.Error("a token that never owned the decision reopened admission")
	}

	if _, err := r.enterTxStart(); !errors.Is(err, ErrServerStopping) {
		t.Fatalf("admission is OPEN after the loser's failed audit: err=%v", err)
	}

	// CONTROL, and the half that makes the assertion above mean something: the
	// OWNER can still reopen, so "closed" is not simply permanent.
	if !r.reopenTxAdmission(aOwner) {
		t.Fatal("the owner could not reopen its own admission")
	}
	release, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("admission did not reopen for its owner: %v", err)
	}
	release()
}

// THE SOLE-FAILURE REOPEN CONTROL. One decider, whose audit fails, must leave
// the daemon able to begin transactions again -- otherwise a shutdown nobody
// carried out would wedge the server into refusing work forever.
func TestShutdownDecision_ASoleFailedDecisionReopensAdmission(t *testing.T) {
	t.Parallel()
	r := newSessionRegistry(64, 64)

	_, owner := r.closeTxAdmission()
	if owner == 0 {
		t.Fatal("the only decider was not given ownership")
	}
	if _, err := r.enterTxStart(); !errors.Is(err, ErrServerStopping) {
		t.Fatalf("admission was not closed by the decision: err=%v", err)
	}

	if !r.reopenTxAdmission(owner) { // its audit failed
		t.Fatal("the owner's abort did not reopen admission")
	}
	release, err := r.enterTxStart()
	if err != nil {
		t.Fatalf("a shutdown that never happened left the daemon refusing transactions: %v", err)
	}
	release()

	// And the spent token cannot reopen a LATER decision's admission.
	_, second := r.closeTxAdmission()
	if second == 0 {
		t.Fatal("a fresh decision could not take ownership after the abort")
	}
	if r.reopenTxAdmission(owner) {
		t.Error("a spent token reopened a later decision's admission")
	}
}
