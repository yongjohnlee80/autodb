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
		if err := e.quiesce(context.Background(), s, 150*time.Millisecond); err == nil {
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
