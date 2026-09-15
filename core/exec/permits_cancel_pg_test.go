package exec

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// livePool builds a pool through THE ENGINE'S OWN pgPoolLimits, so the permit
// dialer and the cancel-marking context-watcher handler under test are the
// ones actually installed in production rather than something this file
// reassembles.
// observer is a plain connection OUTSIDE the ledger, used to ask PostgreSQL
// what it actually did.
//
// IT IS THE WHOLE POINT OF THESE CELLS. Asserting only that the client call
// returned an error proves nothing: pgx's DEFAULT context-watcher merely sets
// a deadline on the socket, so the caller sees a prompt failure while the
// server happily runs pg_sleep(30) to completion. An earlier version of this
// file asserted exactly that and PASSED with the cancel handler removed --
// verified by mutation. Only the server can say whether the statement died.
func observer(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_PGURL"))
	if err != nil {
		t.Fatalf("opening the observer connection: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// stillRunning reports whether that backend is still executing a statement.
func stillRunning(t *testing.T, obs *pgx.Conn, pid int32) bool {
	t.Helper()
	var n int
	err := obs.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND state = 'active' AND query LIKE '%pg_sleep%'`,
		pid).Scan(&n)
	if err != nil {
		t.Fatalf("querying pg_stat_activity: %v", err)
	}
	return n > 0
}

// awaitTerminated waits for the server to stop running that statement.
func awaitTerminated(t *testing.T, obs *pgx.Conn, pid int32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !stillRunning(t, obs, pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("backend %d is STILL running pg_sleep after %v — the client call returned, but "+
		"PostgreSQL never received a cancel. A deadline on the socket is not a cancellation: "+
		"the statement goes on holding its locks and its connection", pid, within)
}

func livePool(t *testing.T, budget, poolMax int) (*pgxpool.Pool, *permitLedger) {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping live cancellation test")
	}

	e := New(nil, nil, WithTargetConnBudget(budget), WithPoolLimits(poolMax, 0, 0))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing TEST_PGURL: %v", err)
	}
	e.pgPoolLimits(nil)(cfg)

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening the pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, e.targetPermits
}

// saturate takes every ORDINARY slot, so the only capacity left is the
// reserved control lane.
func saturate(t *testing.T, pool *pgxpool.Pool, l *permitLedger) []*pgxpool.Conn {
	t.Helper()
	var held []*pgxpool.Conn
	ordinary := l.Snapshot().OrdinaryLimit
	for len(held) < ordinary {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := pool.Acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("acquiring ordinary connection %d of %d: %v", len(held)+1, ordinary, err)
		}
		// Force the physical dial: an acquired-but-unused pool entry may not
		// have opened a socket, and it is the SOCKET the ledger counts.
		if _, err := c.Exec(context.Background(), "SELECT 1"); err != nil {
			t.Fatalf("priming connection %d: %v", len(held)+1, err)
		}
		held = append(held, c)
	}
	t.Cleanup(func() {
		for _, c := range held {
			c.Release()
		}
	})
	if got := l.Snapshot().Outstanding; got != ordinary {
		t.Fatalf("outstanding = %d after saturating, want %d", got, ordinary)
	}
	return held
}

// A cancellation must reach PostgreSQL when every ordinary slot is spent --
// which is exactly when a developer reaches for it, because the system is
// busy. Before the reserved lane existed, the cancel dial was charged to the
// ordinary allowance and refused here.
func TestLivePG_CancellationWorksAtOrdinarySaturation(t *testing.T) {
	pool, ledger := livePool(t, 6, 16) // 5 ordinary + 1 reserved
	held := saturate(t, pool, ledger)

	// The cancel travels on its own socket; the query runs on a held one.
	victim := held[0]
	obs := observer(t)
	peak := newPeakWatcher(ledger)
	defer peak.stop()

	var pid int32
	if err := victim.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("reading the backend pid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := victim.Exec(ctx, "SELECT pg_sleep(30)")
		errCh <- err
	}()

	time.Sleep(500 * time.Millisecond) // let the statement reach the server
	if !stillRunning(t, obs, pid) {
		t.Fatal("the statement was not running before the cancel, so this proves nothing")
	}
	cancel()

	// THE ASSERTION THAT MATTERS: the SERVER stopped, on that EXACT backend.
	awaitTerminated(t, obs, pid, 10*time.Second)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("pg_sleep(30) returned success; it was never cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client call never returned")
	}

	if over := peak.max(); over > 6 {
		t.Errorf("peak live-plus-in-flight = %d, Configured = 6 — the control lane must be "+
			"COUNTED, not an exemption that overshoots the operator's number", over)
	}
}

// The same, during a 50->25 drain. Outstanding is above the configured number
// by definition while draining, so a control lane that re-tested against it
// would refuse a cancel precisely when the system is most loaded.
func TestLivePG_CancellationWorksDuringADrain(t *testing.T) {
	pool, ledger := livePool(t, 10, 24) // 9 ordinary + 1 reserved
	held := saturate(t, pool, ledger)

	// Lower the budget under the live sockets: nothing is killed, and no new
	// ordinary dial is granted.
	if err := ledger.SetBudget(4); err != nil {
		t.Fatal(err)
	}
	if s := ledger.Snapshot(); !s.Draining {
		t.Fatalf("expected a draining ledger, got %+v", s)
	}

	victim := held[0]
	obs := observer(t)
	var pid int32
	if err := victim.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("reading the backend pid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := victim.Exec(ctx, "SELECT pg_sleep(30)")
		errCh <- err
	}()

	time.Sleep(500 * time.Millisecond)
	if !stillRunning(t, obs, pid) {
		t.Fatal("the statement was not running before the cancel, so this proves nothing")
	}
	cancel()

	awaitTerminated(t, obs, pid, 10*time.Second)
	<-errCh
}

// Two cancellations at once serialize on the single lane rather than one being
// lost: the caller has already given up on their query and has no way to learn
// the cancel never went.
func TestLivePG_ConcurrentCancellationsSerialize(t *testing.T) {
	pool, ledger := livePool(t, 8, 24) // 7 ordinary + 1 reserved
	held := saturate(t, pool, ledger)

	peak := newPeakWatcher(ledger)
	defer peak.stop()

	obs := observer(t)
	pids := make([]int32, 2)
	for i := range 2 {
		if err := held[i].QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pids[i]); err != nil {
			t.Fatalf("reading backend pid %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := held[i].Exec(ctx, "SELECT pg_sleep(30)")
				done <- err
			}()
			time.Sleep(500 * time.Millisecond)
			cancel()
			select {
			case errs[i] = <-done:
			case <-time.After(20 * time.Second):
				errs[i] = errors.New("never returned")
			}
		}()
	}
	wg.Wait()

	// BOTH must have been cancelled server-side. If the lane dropped the
	// second one, its backend is still sleeping.
	for i, pid := range pids {
		awaitTerminated(t, obs, pid, 10*time.Second)
		if errs[i] == nil {
			t.Errorf("cancel %d: the statement returned success", i)
		}
	}
	if over := peak.max(); over > 8 {
		t.Errorf("peak = %d, Configured = 8 — concurrent cancels must share ONE lane", over)
	}
	// The lane is free again afterwards: a permit held by a finished cancel is
	// a slot lost until restart.
	if _, err := ledger.AcquireControl(context.Background()); err != nil {
		t.Errorf("the control lane was not released after the cancels finished: %v", err)
	}
}

// peakWatcher samples outstanding so a test can assert the budget was never
// exceeded, rather than only checking the state it happens to end in.
type peakWatcher struct {
	mu   sync.Mutex
	peak int
	done chan struct{}
}

func newPeakWatcher(l *permitLedger) *peakWatcher {
	w := &peakWatcher{done: make(chan struct{})}
	go func() {
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-tick.C:
				n := l.Snapshot().Outstanding
				w.mu.Lock()
				if n > w.peak {
					w.peak = n
				}
				w.mu.Unlock()
			}
		}
	}()
	return w
}

func (w *peakWatcher) stop() { close(w.done) }

func (w *peakWatcher) max() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak
}
