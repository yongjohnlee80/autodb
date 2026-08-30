package exec

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// controllableTx observes what a teardown actually does. It records whether
// RollbackContext was ever issued WHILE a statement was still executing on
// the same connection — the condition a live driver may happen to tolerate
// and therefore hide.
type controllableTx struct {
	active     atomic.Bool
	rolledBack atomic.Bool
	sawOverlap atomic.Bool
	release    chan struct{}
}

func newControllableTx() *controllableTx {
	return &controllableTx{release: make(chan struct{})}
}

// run models the in-flight statement. honourCancel says whether it stops when
// its context is cancelled; the ignoring variant is what proves the join is
// checked rather than merely waited out.
func (c *controllableTx) run(ctx context.Context, honourCancel bool) {
	c.active.Store(true)
	defer c.active.Store(false)
	if honourCancel {
		select {
		case <-ctx.Done():
		case <-c.release:
		}
		return
	}
	<-c.release
}

func (c *controllableTx) RollbackContext(context.Context) error {
	if c.active.Load() {
		c.sawOverlap.Store(true)
	}
	c.rolledBack.Store(true)
	return nil
}
func (c *controllableTx) CommitContext(context.Context) error { return nil }
func (c *controllableTx) ExecContext(context.Context, string, ...any) (dao.Result, error) {
	return nopResult{}, nil
}
func (c *controllableTx) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return nil, errors.New("controllableTx: no queries")
}
func (c *controllableTx) Commit() error   { return nil }
func (c *controllableTx) Rollback() error { return nil }

var _ dao.TxConn = (*controllableTx)(nil)

// sessionWithInFlight builds a session holding tx with a statement running on
// it, and returns a function that waits for that statement to return.
func sessionWithInFlight(t *testing.T, tx *controllableTx, honourCancel bool) (*session, func()) {
	t.Helper()

	sctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &session{id: "lifecycle", userID: 1, connID: 1, ctx: sctx, cancel: cancel}
	s.state.Store(int32(sessOpen))
	s.tx, s.txPhase, s.txID = tx, txActive, "tx-1"
	s.limits = defaultTxLimits()

	if err := s.begin(); err != nil {
		t.Fatalf("claiming the statement slot: %v", err)
	}
	runCtx, endRun := s.runContext(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.finish()
		defer endRun()
		tx.run(runCtx, honourCancel)
	}()
	return s, wg.Wait
}

// MF7. The timeout path took the transaction out from under whatever was
// running and rolled it back concurrently — two commands in flight on one
// connection. A live pgx driver happened to mask it; a controllable one does
// not.
func TestRollbackExpired_QuiescesBeforeRollingBack(t *testing.T) {
	t.Parallel()

	tx := newControllableTx()
	s, join := sessionWithInFlight(t, tx, true)

	e := newFixture(t).eng
	e.rollbackExpired(context.Background(), s, "tx-1", "idle-in-tx")
	// Release the statement AFTER the teardown has run, so the assertions
	// below describe what happened while it was live — and so a failing run
	// fails rather than deadlocking on its own join. A test that hangs when
	// the code is wrong reports nothing useful.
	close(tx.release)
	join()

	if !tx.rolledBack.Load() {
		t.Fatal("the expired transaction was never rolled back; it keeps holding locks on the target")
	}
	if tx.sawOverlap.Load() {
		t.Fatal("ROLLBACK was issued while a statement was still executing on the same connection — " +
			"two commands in flight at once, which is undefined at the protocol level")
	}
	if s.txPhase != txNone {
		t.Errorf("the session still reports phase %v after the rollback", s.txPhase)
	}
}

// And the join is CHECKED, not merely waited out. A statement that ignores
// cancellation must not be rolled back underneath: the previous code waited
// its ten seconds and then proceeded either way, which is a pause, not a join.
func TestRollbackExpired_WillNotRollBackUnderALiveStatement(t *testing.T) {
	t.Parallel()

	tx := newControllableTx()
	s, join := sessionWithInFlight(t, tx, false) // ignores cancellation
	defer func() { close(tx.release); join() }()

	e := newFixture(t).eng
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A short bound: the production value is a real wait, and this test
		// is about what happens WHEN it elapses.
		if _, err := e.quiesce(context.Background(), s, 150*time.Millisecond); err == nil {
			t.Error("quiesce reported success while the statement was still running")
			return
		}
		e.rollbackExpired(context.Background(), s, "tx-1", "idle-in-tx")
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown blocked on a statement that ignores cancellation")
	}

	if tx.rolledBack.Load() {
		t.Fatal("ROLLBACK was issued while a statement that ignores cancellation was still " +
			"executing; the wait elapsed and the teardown proceeded anyway")
	}
	if s.txPhase == txNone {
		t.Error("the transaction was detached without being rolled back, so nothing will ever end it")
	}
}

// MF4. When the statement will not stop, closeSession correctly declines to
// roll back concurrently — and then dropped the session anyway. The
// transaction stayed attached to an object no longer in the registry, so
// nothing could retry it, nothing accounted for it, and conn.delete's pool
// close waited on a connection no reachable owner held.
//
// Skipping the rollback is the right call. Losing the owner is not.
func TestCloseSession_RetainsTheOwnerWhenTheRollbackIsSkipped(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	e := f.eng
	tx := newControllableTx()
	s, join := sessionWithInFlight(t, tx, false) // ignores cancellation
	s.connID = f.connID
	if err := e.sessions.admit(s); err != nil {
		t.Fatalf("admitting the session: %v", err)
	}

	// A short bound so the test is about what happens WHEN it elapses.
	origin := closeQuiesceTimeoutForTest
	closeQuiesceTimeoutForTest = 150 * time.Millisecond
	defer func() { closeQuiesceTimeoutForTest = origin }()

	e.closeSession(context.Background(), s, testIP, "connection-deleted")

	if tx.rolledBack.Load() {
		t.Fatal("ROLLBACK was issued underneath a statement that would not stop")
	}
	// THE EFFECT: the transaction still has an owner, and that owner is
	// still reachable from the registry.
	if s.get() != sessClosing {
		t.Errorf("session state is %v, want closing — a session whose rollback was skipped "+
			"must stay in a state the janitor will retry", s.get())
	}
	found := false
	for _, got := range e.sessions.snapshot() {
		if got.id == s.id {
			found = true
		}
	}
	if !found {
		t.Fatal("the session was removed from the registry with its transaction still attached — " +
			"nothing can retry the rollback, nothing accounts for the held connection, and " +
			"conn.delete's pool close waits on a connection no reachable owner holds")
	}
	if s.tx == nil {
		t.Error("the transaction was detached without being rolled back, so nothing will end it")
	}

	// And the retry actually ends it once the statement finally stops.
	close(tx.release)
	join()
	if n := e.reapExpired(context.Background(), time.Now()); n == 0 {
		t.Fatal("the janitor did not retry the retained closing session")
	}
	if !tx.rolledBack.Load() {
		t.Error("the retry did not roll the transaction back")
	}
	if s.get() != sessClosed {
		t.Errorf("session state after the retry is %v, want closed", s.get())
	}
}

// The other half of MF4: nothing may start on the transaction between the
// join that proved the session idle and the detach that ends it. Proving a
// fact and then acting on it a moment later is how a statement ends up
// running on a transaction being rolled back out from under it.
func TestQuiesce_HoldsTheSlotUntilTheTeardownIsDone(t *testing.T) {
	t.Parallel()

	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{id: "slot", ctx: sctx, cancel: cancel}
	s.state.Store(int32(sessOpen))

	e := newFixture(t).eng
	release, err := e.quiesce(context.Background(), s, time.Second)
	if err != nil {
		t.Fatalf("quiescing an idle session: %v", err)
	}

	// While the teardown holds the slot, a statement cannot claim it.
	if err := s.begin(); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("a statement claimed the session mid-teardown (%v); it would run on a "+
			"transaction that is being rolled back out from under it", err)
	}

	release()
	if err := s.begin(); err != nil {
		t.Errorf("the slot was not released after the teardown: %v", err)
	}
	s.finish()
}
