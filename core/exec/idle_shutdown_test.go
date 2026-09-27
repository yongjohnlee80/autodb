package exec

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// idle_shutdown_test.go holds the idle-only shutdown's one decision: it counts
// transactions, statements and wire clients under their gates, closes all
// three only when every count is zero, and an abort reopens all three.

func TestAnIdleShutdownClosesAllThreeGatesAndAnAbortReopensThem(t *testing.T) {
	f := newFixture(t)
	counts, owner := f.eng.BeginIdleShutdown()
	if owner == 0 || counts.Busy() {
		t.Fatalf("an idle engine: counts %+v, owner %d; want no counts and an owner", counts, owner)
	}
	if _, err := f.eng.enterStatement(); !errors.Is(err, ErrStatementAdmissionClosed) {
		t.Errorf("a statement after the decision: %v, want ErrStatementAdmissionClosed", err)
	}
	if _, ok := f.eng.AdmitWireConnection(); ok {
		t.Error("a wire client was admitted after the decision")
	}
	if _, err := f.eng.sessions.enterTxStart(); !errors.Is(err, ErrServerStopping) {
		t.Errorf("a BEGIN after the decision: %v, want ErrServerStopping", err)
	}
	// The abort reopens ALL THREE, only for its owner.
	if f.eng.AbortIdleShutdown(owner + 1) {
		t.Error("a stale owner token reopened the gates")
	}
	if !f.eng.AbortIdleShutdown(owner) {
		t.Fatal("the owner could not reopen the gates")
	}
	leave, err := f.eng.enterStatement()
	if err != nil {
		t.Errorf("a statement after the abort: %v", err)
	} else {
		leave()
	}
	if release, ok := f.eng.AdmitWireConnection(); !ok {
		t.Error("a wire client refused after the abort")
	} else {
		release()
	}
	if release, err := f.eng.sessions.enterTxStart(); err != nil {
		t.Errorf("a BEGIN after the abort: %v", err)
	} else {
		release()
	}
}

// The existing AbortShutdown reopens the transaction gate ALONE; using it for
// an idle decision would leave the daemon refusing every statement for good.
func TestAbortShutdownIsNotAnIdleAbort(t *testing.T) {
	f := newFixture(t)
	_, owner := f.eng.BeginIdleShutdown()
	f.eng.AbortShutdown(owner)
	if _, err := f.eng.enterStatement(); !errors.Is(err, ErrStatementAdmissionClosed) {
		t.Fatalf("after AbortShutdown the statement gate is %v — this cell pins that only "+
			"AbortIdleShutdown reopens it, so a caller must use that one", err)
	}
	f.eng.AbortIdleShutdown(owner)
}

// Each of the three makes the engine busy, and a busy decision closes nothing.
func TestAnyRunningThingMakesItBusyAndClosesNothing(t *testing.T) {
	f := newFixture(t)
	check := func(what string, want IdleCounts) {
		t.Helper()
		counts, owner := f.eng.BeginIdleShutdown()
		if owner != 0 || counts != want {
			t.Fatalf("%s: counts %+v owner %d; want %+v and no owner", what, counts, owner, want)
		}
		leave, err := f.eng.enterStatement() // nothing closed
		if err != nil {
			t.Fatalf("%s: a busy decision closed the statement gate: %v", what, err)
		}
		leave()
	}
	leave, err := f.eng.enterStatement()
	if err != nil {
		t.Fatal(err)
	}
	check("a statement executing", IdleCounts{Executing: 1})
	leave()
	release, ok := f.eng.AdmitWireConnection()
	if !ok {
		t.Fatal("wire admission refused")
	}
	check("a wire client connected", IdleCounts{WireSessions: 1})
	release()
}

// An open transaction makes it busy — on PostgreSQL, where a session holds one
// across calls (pgWireSession opens with one).
func TestAnOpenTransactionMakesItBusy(t *testing.T) {
	f, _, _, _, _ := pgWireSession(t)
	counts, owner := f.eng.BeginIdleShutdown()
	if owner != 0 || counts.InTransaction != 1 {
		t.Fatalf("with a transaction open: counts %+v owner %d; want one in transaction and no owner", counts, owner)
	}
}

// ONE DECISION: statements hammering the gate while decisions are taken never
// see a decision that owns the gates with a statement admitted after it — the
// window between counting and closing does not exist.
func TestNoStatementStartsBetweenTheCountAndTheClose(t *testing.T) {
	f := newFixture(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	admittedAfter := 0
	var closedAt time.Time
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				leave, err := f.eng.enterStatement()
				if err != nil {
					continue
				}
				mu.Lock()
				if !closedAt.IsZero() {
					admittedAfter++
				}
				mu.Unlock()
				time.Sleep(200 * time.Microsecond) // a statement takes time: the window is real
				leave()
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counts, owner := f.eng.BeginIdleShutdown(); owner != 0 {
			// At the instant of the close, nothing may be running: the
			// decision said none was, and the drain would destroy it.
			f.eng.idle.mu.Lock()
			running := f.eng.idle.executing
			f.eng.idle.mu.Unlock()
			if running != 0 {
				t.Errorf("%d statement(s) running at the close of a decision that counted none", running)
			}
			mu.Lock()
			closedAt = time.Now()
			mu.Unlock()
			if counts.Busy() {
				t.Errorf("an owner with counts %+v", counts)
			}
			time.Sleep(20 * time.Millisecond) // statements keep trying
			break
		}
	}
	close(stop)
	wg.Wait()
	if closedAt.IsZero() {
		t.Skip("no idle instant was found under this load; the cell observed nothing")
	}
	if admittedAfter != 0 {
		t.Errorf("%d statement(s) were admitted after the decision closed the gate", admittedAfter)
	}
}
