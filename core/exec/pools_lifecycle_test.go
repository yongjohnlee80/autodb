package exec

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// LIFECYCLE RACES. Each cell drives the competing write INSIDE the window it
// could break (a hook), rather than running it alongside and hoping. The
// schedule comes from the acquisition itself: lifecycleOp reports whether a
// second writer found the guard free or held, before it waits. A writer that
// found it free (no guard) is let run to the end while the first is held,
// which is exactly the overtaking the guard exists to stop. Timers are
// watchdogs only, never evidence.

// watchdog bounds every wait in these cells.
const watchdog = 10 * time.Second // not a timeout: a test's watchdog

// recv waits for ch, failing the cell if the watchdog runs out first.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("waited %v for %s", watchdog, what)
	}
	panic("unreachable")
}

// lifecycleProbe holds the first `holds` lifecycle writes that reach step,
// the n-th until release[n-1] is closed, and reports each arrival there on
// arrived. Every acquisition of the guard is reported on free: true when the
// guard was free, false when the writer found it held. Installed after a
// cell's setup, so only the cell's own writes report.
type lifecycleProbe struct {
	n       atomic.Int32
	arrived chan int32
	free    chan bool
	release []chan struct{}
}

func probeLifecycle(e *Engine, step string, holds int) *lifecycleProbe {
	p := &lifecycleProbe{arrived: make(chan int32, 4), free: make(chan bool, 4)}
	for range holds {
		p.release = append(p.release, make(chan struct{}))
	}
	e.hookInLifecycleOp = func(at string) {
		switch at {
		case lifecycleAcquired:
			p.free <- true
		case lifecycleBlocked:
			p.free <- false
		case step:
			n := p.n.Add(1)
			p.arrived <- n
			if int(n) <= len(p.release) {
				<-p.release[n-1]
			}
		}
	}
	return p
}

// first waits for the first writer to take the guard and reach step.
func (p *lifecycleProbe) first(t *testing.T) {
	t.Helper()
	if !recv(t, p.free, "the first write to take the guard") {
		t.Fatal("the first write found the guard held; nothing else should hold it")
	}
	recv(t, p.arrived, "the first write to reach its held step")
}

// AN OPEN THAT OUTLIVES A MOVE IS REBUILT ON THE CONNECTION'S POOL AFTER IT.
// target() opens a pgxpool outside the engine's lock, and pgPoolLimits
// captures the ledger at that moment. A whole move landing in between ends
// its drain before the open publishes, so a drain check alone sees nothing:
// without the revocation the pool built on the old ledger is cached and keeps
// charging it for every new socket. A failed move revokes too, and the
// rebuild lands back on the pool the connection never left.
func TestTarget_AnOpenThatOutlivesAMoveIsRebuiltOnTheCurrentPool(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; the open needs a live PostgreSQL target")
	}
	for _, tc := range []struct {
		name     string
		moveFail bool
	}{
		{"the move commits", false},
		{"the move fails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			e := f.eng
			pg, err := e.CreateConnection(ctx, f.rootTok, "pg-move", "postgres", dsn, testIP)
			if err != nil {
				t.Fatalf("CreateConnection: %v", err)
			}
			moveTo, err := e.CreatePool(ctx, f.rootTok, "destination", 10, testIP)
			if err != nil {
				t.Fatal(err)
			}
			row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, pg).Get()
			if err != nil {
				t.Fatal(err)
			}

			var first dao.DataConn
			var moveErr error
			var once sync.Once
			e.hookBeforePublish = func(c dao.DataConn) {
				once.Do(func() {
					first = c
					mctx, cancel := context.WithCancel(ctx)
					defer cancel()
					if tc.moveFail {
						// Fail the move's write after its drain began.
						e.hookInLifecycleOp = func(step string) {
							if step == "conn.set_pool drained" {
								cancel()
							}
						}
					}
					moveErr = e.SetConnectionPool(mctx, f.rootTok, pg, moveTo, testIP)
					e.hookInLifecycleOp = nil
				})
			}
			got, err := e.target(ctx, pg, row)
			e.hookBeforePublish = nil
			if err != nil {
				t.Fatalf("target: %v", err)
			}
			if first == nil {
				t.Fatal("the hook never ran: no open was in flight, so this cell proves nothing")
			}
			if tc.moveFail != (moveErr != nil) {
				t.Fatalf("the move returned %v; this case wants it to %s", moveErr,
					map[bool]string{true: "fail", false: "commit"}[tc.moveFail])
			}

			want, other := moveTo, int64(meta.DefaultPoolID)
			if tc.moveFail {
				want, other = other, want
			}
			if got == first {
				t.Fatalf("target published the pool it opened before the move ended; it was built "+
					"on the ledger of the lifecycle the move replaced and charges it for every new "+
					"socket (want a rebuild charging pool %d)", want)
			}
			if rows, qerr := first.QueryContext(ctx, "SELECT 1"); qerr == nil {
				_ = rows.Close()
				t.Error("the obsolete pool still answers; it was never closed")
			}

			// The rebuilt pool charges the pool the connection is in now:
			// hold a socket and read both ledgers.
			done := make(chan struct{})
			go func() {
				defer close(done)
				if rows, qerr := got.QueryContext(ctx, "SELECT pg_sleep(1)"); qerr == nil {
					_ = rows.Close()
				}
			}()
			// Sampled until the statement's socket shows and the obsolete
			// pool's probe socket has gone, or the window runs out.
			var wantN, otherN int
			for deadline := time.Now().Add(watchdog); time.Now().Before(deadline); {
				wantN, otherN = poolOf(t, f, want).Ledger.Outstanding, poolOf(t, f, other).Ledger.Outstanding
				if wantN > 0 && otherN == 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			<-done
			if wantN == 0 || otherN != 0 {
				t.Errorf("a statement on the published pool charged pool %d %d socket(s) and pool %d "+
					"%d; want it charged to pool %d alone", want, wantN, other, otherN, want)
			}
		})
	}
}

// TWO UPDATES PUBLISH IN THE ORDER THEY COMMIT. The first update is held
// between its commit and its live publication while a second runs: without
// one lifecycle at a time, the second commits 3 and publishes 3, then the
// first publishes 95 over it, and the ledger grants against 95 while the
// store says 3.
func TestLifecycle_TwoUpdatesPublishInCommitOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	id, err := f.eng.CreatePool(ctx, f.rootTok, "ordered", 25, testIP)
	if err != nil {
		t.Fatal(err)
	}
	probe := probeLifecycle(f.eng, "pool.update committed", 1)

	doneA, doneB := make(chan struct{}), make(chan struct{})
	var errA, errB error
	go func() { defer close(doneA); errA = f.eng.UpdatePool(ctx, f.rootTok, id, 95, testIP) }()
	probe.first(t)
	go func() { defer close(doneB); errB = f.eng.UpdatePool(ctx, f.rootTok, id, 3, testIP) }()
	if recv(t, probe.free, "the second update to reach the guard") {
		recv(t, doneB, "the second update, which found the guard free, to finish")
	}
	close(probe.release[0])
	recv(t, doneA, "the first update to finish")
	recv(t, doneB, "the second update to finish")
	if errA != nil || errB != nil {
		t.Fatalf("UpdatePool: %v, %v", errA, errB)
	}

	stored, err := f.store.Pools.OnCtx(ctx).With(meta.PoolID, id).Get()
	if err != nil {
		t.Fatal(err)
	}
	fresh := New(f.store, f.svc)
	t.Cleanup(func() { _ = fresh.Close() })
	if err := fresh.LoadPools(ctx); err != nil {
		t.Fatal(err)
	}
	var reloaded int
	for _, p := range fresh.pools.snapshot() {
		if p.ID == id {
			reloaded = p.Ledger.Configured
		}
	}
	live := poolOf(t, f, id).Ledger.Configured
	if stored.MaxConns != 3 || live != 3 || reloaded != 3 {
		t.Errorf("after 95 then 3: stored %d, live %d, a fresh load %d; all three must be 3",
			stored.MaxConns, live, reloaded)
	}
	got := auditDetail(t, f, "pool_updated")
	want := []string{"max_conns 25 -> 95", "max_conns 95 -> 3"}
	if len(got) != len(want) {
		t.Fatalf("pool_updated rows = %q, want two", got)
	}
	for i := range want {
		if !strings.HasSuffix(got[i], want[i]) {
			t.Errorf("pool_updated row %d = %q, want it to end %q", i, got[i], want[i])
		}
	}
}

// TWO MOVES OF ONE CONNECTION DO NOT SHARE A FENCE. The first move is held
// inside its drain while a second runs: without one lifecycle at a time both
// set the same mark, the first clears it on its way out, and the second is
// left mid-move with nothing refusing admission.
func TestLifecycle_TwoMovesOfOneConnectionDoNotShareAFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	pg := pgConn(t, f, "moved-twice")
	b, err := f.eng.CreatePool(ctx, f.rootTok, "b", 10, testIP)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.eng.CreatePool(ctx, f.rootTok, "c", 10, testIP)
	if err != nil {
		t.Fatal(err)
	}
	// Both moves are held inside their drains: the first while the second
	// tries to overtake it, the second while admission is read.
	probe := probeLifecycle(f.eng, "conn.set_pool drained", 2)

	done1, done2 := make(chan struct{}), make(chan struct{})
	var err1, err2 error
	go func() { defer close(done1); err1 = f.eng.SetConnectionPool(ctx, f.rootTok, pg, b, testIP) }()
	probe.first(t)
	go func() { defer close(done2); err2 = f.eng.SetConnectionPool(ctx, f.rootTok, pg, c, testIP) }()
	if recv(t, probe.free, "the second move to reach the guard") {
		// Found free: it drains alongside the first, which then finishes.
		recv(t, probe.arrived, "the second move to reach its drain")
		close(probe.release[0])
		recv(t, done1, "the first move to finish")
	} else {
		close(probe.release[0])
		recv(t, done1, "the first move to finish")
		recv(t, probe.arrived, "the second move to reach its drain")
	}
	// The second move is inside its drain now, and held there.

	if derr := f.eng.sessions.drainErr(pg); !errors.Is(derr, ErrConnectionDraining) {
		t.Errorf("mid-move, admission answers %v, want ErrConnectionDraining: the other move "+
			"lifted this one's fence", derr)
	}
	close(probe.release[1])
	recv(t, done2, "the second move to finish")
	if err1 != nil || err2 != nil {
		t.Fatalf("SetConnectionPool: %v, %v", err1, err2)
	}
	if key, _ := f.eng.pools.leaseScope(pg); key != (leaseKey{pool: c}) {
		t.Errorf("after moves to %d then %d, the connection scopes to %+v", b, c, key)
	}
	if derr := f.eng.sessions.drainErr(pg); derr != nil {
		t.Errorf("both moves are done and the connection is still draining: %v", derr)
	}
}

// A DELETE AND AN ASSIGNMENT DO NOT LEAVE A CONNECTION IN A DELETED POOL. The
// delete is held between its membership count and its write while a move into
// the pool runs. SQLite's schema has no foreign key to refuse the dangling
// assignment, so only the order of the two writes does.
func TestLifecycle_ADeleteAndAnAssignmentLeaveNoDanglingMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	pg := pgConn(t, f, "assigned")
	p, err := f.eng.CreatePool(ctx, f.rootTok, "doomed", 10, testIP)
	if err != nil {
		t.Fatal(err)
	}
	probe := probeLifecycle(f.eng, "pool.delete counted", 1)

	doneD, doneM := make(chan struct{}), make(chan struct{})
	var errD, errM error
	go func() { defer close(doneD); errD = f.eng.DeletePool(ctx, f.rootTok, p, testIP) }()
	probe.first(t)
	go func() { defer close(doneM); errM = f.eng.SetConnectionPool(ctx, f.rootTok, pg, p, testIP) }()
	if recv(t, probe.free, "the move to reach the guard") {
		recv(t, doneM, "the move, which found the guard free, to finish")
	}
	close(probe.release[0])
	recv(t, doneD, "the delete to finish")
	recv(t, doneM, "the move to finish")
	if errD != nil {
		t.Fatalf("DeletePool: %v", errD)
	}
	if !errors.Is(errM, ErrInvalidPool) {
		t.Errorf("a move into a pool deleted first = %v, want ErrInvalidPool", errM)
	}

	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, pg).Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := f.store.Pools.OnCtx(ctx).With(meta.PoolID, row.PoolID).Get(); perr != nil {
		t.Errorf("the stored connection is in pool %d, which does not exist (%v)", row.PoolID, perr)
	}
	if key, _ := f.eng.pools.leaseScope(pg); key.pool == p {
		t.Errorf("the live connection is in the deleted pool %d", p)
	}
}

// A CLAMP WHOSE AUDIT ROW FAILS STORES NOTHING. The blob is upserted first and
// the audit row second, in one transaction: a failure at the second rolls the
// first back, and the engine runs under the stored generation.
func TestLoadDurablePolicy_AClampWhoseAuditFailsStoresNothing(t *testing.T) {
	t.Parallel()
	f := policyFixture(t)
	f.eng.hookAuditFail = func(action string) error {
		if action == "policy_clamped" {
			return errors.New("injected: the audit write failed")
		}
		return nil
	}

	got := f.eng.persistClampedPolicy(context.Background(), goodSpec(), 4, []string{"max_tx_duration 8h0m0s -> 2h0m0s"})
	if got != 4 {
		t.Errorf("generation = %d after the audit failed, want the stored 4", got)
	}
	if _, ok, _ := f.store.GetMeta(context.Background(), policyStoreKey); ok {
		t.Error("the blob was stored without its policy_clamped row; the two go together")
	}
	if n := len(auditDetail(t, f, "policy_clamped")); n != 0 {
		t.Errorf("%d policy_clamped rows after an injected failure", n)
	}
}
