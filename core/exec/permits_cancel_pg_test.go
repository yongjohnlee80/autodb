package exec

import (
	"context"
	"errors"
	"net"
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

// awaitRunning waits for the statement to reach the server. A fixed sleep
// either wastes time or races on a loaded machine, and racing here makes the
// cell prove nothing: cancelling a statement that has not started is not the
// case under test.
func awaitRunning(t *testing.T, obs *pgx.Conn, pid int32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if stillRunning(t, obs, pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend %d never started the statement within %v", pid, within)
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

// dialBarrier sits UNDER the production wrapper: pgPoolLimits wraps whatever
// DialFunc it finds, so a barrier installed first runs AFTER the permit is
// acquired and BEFORE the socket opens.
//
// THAT WINDOW IS THE ONLY PLACE THE CLAIM CAN BE CHECKED. Sampling the ledger
// on a ticker can miss a control socket that lives for a few milliseconds --
// and a poll that misses it passes whether the socket was counted, uncounted,
// or never taken at all. Holding the dial still makes the assertion exact.
type dialBarrier struct {
	mu      sync.Mutex
	gate    chan struct{}
	entered chan string
	arm     bool
}

func newDialBarrier() *dialBarrier {
	return &dialBarrier{gate: make(chan struct{}), entered: make(chan string, 8)}
}

func (b *dialBarrier) armFor(on bool) {
	b.mu.Lock()
	b.arm = on
	b.mu.Unlock()
}

func (b *dialBarrier) dialFunc() func(context.Context, string, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		b.mu.Lock()
		armed := b.arm
		b.mu.Unlock()
		if armed && isControlDial(ctx) {
			b.entered <- addr
			select {
			case <-b.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return d.DialContext(ctx, network, addr)
	}
}

func (b *dialBarrier) release() { close(b.gate) }

func livePoolWithBarrier(t *testing.T, budget, poolMax int) (*pgxpool.Pool, *permitLedger, *dialBarrier) {
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
	b := newDialBarrier()
	cfg.ConnConfig.DialFunc = b.dialFunc() // UNDER the production wrapper
	e.pgPoolLimits(nil)(cfg)

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening the pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, e.targetPermits, b
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

	awaitRunning(t, obs, pid, 10*time.Second)
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

	awaitRunning(t, obs, pid, 10*time.Second)
	cancel()

	awaitTerminated(t, obs, pid, 10*time.Second)
	<-errCh
}

// Both concurrent cancellations are DELIVERED -- neither is silently dropped
// while the lane is busy.
//
// It does NOT claim to prove serialization: two statements eventually stopping
// is equally consistent with two simultaneous control sockets, which would
// overshoot the operator's number. That claim belongs to
// TestLivePG_OnlyOneControlDialEntersAtATime, which holds the first dial still
// and shows the second has not entered.
func TestLivePG_BothConcurrentCancellationsAreDelivered(t *testing.T) {
	pool, ledger := livePool(t, 8, 24) // 7 ordinary + 1 reserved
	held := saturate(t, pool, ledger)

	// EACH GOROUTINE GETS ITS OWN OBSERVER. A pgx conn is not safe for
	// concurrent use, and sharing one here failed with "conn busy" -- a test
	// failing for its own defect rather than the code's.
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
			obs := observer(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := held[i].Exec(ctx, "SELECT pg_sleep(30)")
				done <- err
			}()
			awaitRunning(t, obs, pids[i], 10*time.Second)
			cancel()
			select {
			case errs[i] = <-done:
			case <-time.After(20 * time.Second):
				errs[i] = errors.New("never returned")
			}
			awaitTerminated(t, obs, pids[i], 15*time.Second)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("cancel %d: the statement returned success", i)
		}
	}
	// The lane is free again: a permit held by a finished cancel is a slot
	// lost until restart.
	if _, err := ledger.AcquireControl(context.Background()); err != nil {
		t.Errorf("the control lane was not released after the cancels finished: %v", err)
	}
}

// THE ACCOUNTING, ASSERTED EXACTLY, WITH THE CONTROL DIAL HELD STILL.
//
// A sampling version of this could not prove its own comment: a 2ms ticker can
// miss a socket that lives for a few milliseconds, and "peak <= Configured"
// passes whether that socket was counted, uncounted, or never taken at all.
// Blocking the dial after the permit and before the network open makes every
// number exact.
func TestLivePG_ControlAccountingIsExactWhileTheLaneIsOccupied(t *testing.T) {
	pool, ledger, barrier := livePoolWithBarrier(t, 50, 64)
	held := saturate(t, pool, ledger)
	obs := observer(t)

	// Steady state at Configured 50: O=49, K=0, Effective = max(50, 49+1).
	if s := ledger.Snapshot(); s.Ordinary != 49 || s.Control != 0 ||
		s.Outstanding != 49 || s.Effective != 50 {
		t.Fatalf("steady: O=%d K=%d out=%d eff=%d, want 49/0/49/50",
			s.Ordinary, s.Control, s.Outstanding, s.Effective)
	}

	var pid int32
	if err := held[0].QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = held[0].Exec(ctx, "SELECT pg_sleep(30)") }()
	awaitRunning(t, obs, pid, 10*time.Second)

	barrier.armFor(true)
	cancel()
	select {
	case <-barrier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no marked control dial arrived at the barrier")
	}

	// OCCUPIED, held still: 49 / 1 / 50 / 50.
	s := ledger.Snapshot()
	if s.Ordinary != 49 || s.Control != 1 || s.Outstanding != 50 || s.Effective != 50 {
		t.Errorf("occupied: O=%d K=%d out=%d eff=%d, want 49/1/50/50",
			s.Ordinary, s.Control, s.Outstanding, s.Effective)
	}
	barrier.release()
	awaitTerminated(t, obs, pid, 10*time.Second)
}

// The same three readings across a 50->25 drain: idle, occupied, released. The
// ceiling must not move when the lane fills and empties.
func TestLivePG_DrainAccountingHoldsAcrossCancelOccupancy(t *testing.T) {
	pool, ledger, barrier := livePoolWithBarrier(t, 50, 64)
	held := saturate(t, pool, ledger)
	obs := observer(t)

	if err := ledger.SetBudget(25); err != nil {
		t.Fatal(err)
	}
	idle := ledger.Snapshot()
	if idle.Ordinary != 49 || idle.Control != 0 || idle.Outstanding != 49 || idle.Effective != 50 {
		t.Fatalf("idle lane after 50->25: O=%d K=%d out=%d eff=%d, want 49/0/49/50",
			idle.Ordinary, idle.Control, idle.Outstanding, idle.Effective)
	}

	var pid int32
	if err := held[0].QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = held[0].Exec(ctx, "SELECT pg_sleep(30)") }()
	awaitRunning(t, obs, pid, 10*time.Second)

	barrier.armFor(true)
	cancel()
	select {
	case <-barrier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no marked control dial arrived at the barrier during the drain")
	}

	occ := ledger.Snapshot()
	if occ.Ordinary != 49 || occ.Control != 1 || occ.Outstanding != 50 || occ.Effective != 50 {
		t.Errorf("occupied during drain: O=%d K=%d out=%d eff=%d, want 49/1/50/50",
			occ.Ordinary, occ.Control, occ.Outstanding, occ.Effective)
	}
	if occ.Effective != idle.Effective {
		t.Errorf("the ceiling moved %d -> %d when the lane filled; a drain generation's "+
			"ceiling must not rise and fall with cancel traffic", idle.Effective, occ.Effective)
	}

	barrier.release()
	awaitTerminated(t, obs, pid, 10*time.Second)

	deadline := time.Now().Add(10 * time.Second)
	var rel LedgerSnapshot
	for time.Now().Before(deadline) {
		rel = ledger.Snapshot()
		if rel.Control == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rel.Ordinary != 49 || rel.Control != 0 || rel.Outstanding != 49 || rel.Effective != 50 {
		t.Errorf("released: O=%d K=%d out=%d eff=%d, want 49/0/49/50",
			rel.Ordinary, rel.Control, rel.Outstanding, rel.Effective)
	}
}

// AT MOST ONE marked dial may be in flight, proven by holding the first and
// showing the second has not entered -- not by sampling, which two
// simultaneous uncounted sockets would also pass.
func TestLivePG_OnlyOneControlDialEntersAtATime(t *testing.T) {
	pool, ledger, barrier := livePoolWithBarrier(t, 50, 64)
	held := saturate(t, pool, ledger)
	obs := observer(t)

	pids := make([]int32, 2)
	cancels := make([]context.CancelFunc, 2)
	for i := range 2 {
		if err := held[i].QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pids[i]); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func() { _, _ = held[i].Exec(ctx, "SELECT pg_sleep(30)") }()
		awaitRunning(t, obs, pids[i], 10*time.Second)
	}

	barrier.armFor(true)
	cancels[0]()
	select {
	case <-barrier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first control dial never reached the barrier")
	}

	// The second cancel must NOT enter while the first holds the lane.
	cancels[1]()
	select {
	case <-barrier.entered:
		t.Fatal("a second control dial entered while the first held the lane — the lane " +
			"admits one holder at a time, and two simultaneous sockets would overshoot " +
			"the operator's number")
	case <-time.After(1500 * time.Millisecond):
	}
	if s := ledger.Snapshot(); s.Control != 1 {
		t.Errorf("K = %d while one dial is held, want exactly 1", s.Control)
	}

	// Releasing the first admits the second.
	barrier.release()
	select {
	case <-barrier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second control dial never entered after the lane was released")
	}
	for _, pid := range pids {
		awaitTerminated(t, obs, pid, 15*time.Second)
	}
}
