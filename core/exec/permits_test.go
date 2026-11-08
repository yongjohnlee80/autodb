package exec

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// doneCtx is an acquirer that has already given up: acquire grants a free slot
// at once and otherwise refuses at once, without waiting in the line.
func doneCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// tryAcquire is a non-waiting acquisition of one class.
func tryAcquire(l *permitLedger, class DialClass) (*Permit, error) {
	return l.acquire(dialMark{intent: &dialIntent{class: class}, acquirer: doneCtx()})
}

// THE SPLIT. A pool of 25 is 20 for front-door sessions, 4 for exec, TUI and
// internal work, and 1 for cancels; small pools keep one socket each.
func TestPoolShares(t *testing.T) {
	for _, tc := range []struct{ budget, frontDoor, headroom int }{
		{3, 1, 1}, {4, 1, 2}, {5, 2, 2}, {8, 3, 4}, {10, 5, 4}, {25, 20, 4}, {95, 90, 4},
	} {
		s := newPermitLedger(tc.budget).Snapshot()
		if s.FrontDoorLimit != tc.frontDoor || s.OrdinaryLimit != tc.headroom {
			t.Errorf("budget %d: front door %d, headroom %d; want %d and %d",
				tc.budget, s.FrontDoorLimit, s.OrdinaryLimit, tc.frontDoor, tc.headroom)
		}
		if s.FrontDoorLimit+s.OrdinaryLimit+1 != tc.budget {
			t.Errorf("budget %d: the shares and the lane don't add up to the budget", tc.budget)
		}
	}
}

// EACH SHARE IS BOUNDED BY ITSELF. Exec can't take the front door's sockets,
// and a full front door leaves exec its own.
func TestASharesSocketsAreItsOwn(t *testing.T) {
	l := newPermitLedger(8) // front door 3, headroom 4, lane 1
	var held []*Permit
	for i := range 4 {
		p, err := tryAcquire(l, DialOrdinary)
		if err != nil {
			t.Fatalf("headroom acquire %d: %v", i, err)
		}
		held = append(held, p)
	}
	if _, err := tryAcquire(l, DialOrdinary); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("a fifth ordinary dial must be refused, got %v", err)
	}
	for i := range 3 {
		if _, err := tryAcquire(l, DialFrontDoor); err != nil {
			t.Fatalf("front-door acquire %d with the headroom full: %v — exec took the "+
				"front door's sockets", i, err)
		}
	}
	if _, err := tryAcquire(l, DialFrontDoor); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("a fourth front-door dial must be refused, got %v", err)
	}
	held[0].Release()
	if _, err := tryAcquire(l, DialOrdinary); err != nil {
		t.Fatalf("a released headroom permit must be reusable: %v", err)
	}
	if got := l.Outstanding(); got != 7 {
		t.Errorf("outstanding = %d, want 7", got)
	}
}

// Releasing twice must not manufacture a slot that does not exist.
func TestReleaseIsIdempotent(t *testing.T) {
	l := newPermitLedger(3) // front door 1, headroom 1, lane 1
	p, err := tryAcquire(l, DialOrdinary)
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	p.Release()
	p.Release()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d, want 0 — a double release invented capacity", got)
	}
	if _, err := tryAcquire(l, DialOrdinary); err != nil {
		t.Fatal(err)
	}
	if _, err := tryAcquire(l, DialOrdinary); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("the headroom share must be unchanged after repeated releases")
	}
}

// LOWERING A POOL DRAINS, NEVER KILLS: 51 -> 26 with 46 front-door sockets open.
func TestLoweringTheBudgetDrainsRatherThanKilling(t *testing.T) {
	l := newPermitLedger(51) // front door 46
	var held []*Permit
	for range 46 {
		p, err := tryAcquire(l, DialFrontDoor)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
	}
	_, genBefore := l.Budget()
	if err := l.SetBudget(26); err != nil { // front door 21
		t.Fatal(err)
	}
	if _, genAfter := l.Budget(); genAfter == genBefore {
		t.Error("a budget change must open a new generation; monotonicity is per generation")
	}
	if got := l.Outstanding(); got != 46 {
		t.Fatalf("outstanding = %d, want 46 — lowering the budget must not kill live work", got)
	}
	if s := l.Snapshot(); !s.Draining {
		t.Error("a share above its new limit IS the drain; the snapshot must say so")
	}
	if _, err := tryAcquire(l, DialFrontDoor); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("no new permit may be granted above the lowered share")
	}
	for i := range 26 {
		held[i].Release()
	}
	if got := l.Outstanding(); got != 20 {
		t.Fatalf("outstanding = %d, want 20", got)
	}
	if _, err := tryAcquire(l, DialFrontDoor); err != nil {
		t.Fatalf("once drained below the share a dial must be granted: %v", err)
	}
}

// Concurrent acquirers never collectively exceed their share.
func TestConcurrentAcquiresNeverExceedTheShare(t *testing.T) {
	l := newPermitLedger(8) // headroom 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		peak int
		held []*Permit
	)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := tryAcquire(l, DialOrdinary)
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, p)
			peak = max(peak, len(held))
			mu.Unlock()
		}()
	}
	wg.Wait()
	if peak > 4 {
		t.Fatalf("peak = %d, headroom share = 4 — the ledger over-granted", peak)
	}
	if l.Outstanding() != len(held) {
		t.Fatalf("outstanding = %d, granted-and-held = %d", l.Outstanding(), len(held))
	}
}

// A FULL SHARE IS A LINE, FIRST COME FIRST SERVED: each release goes to the
// oldest waiter of that share, and an arrival never steps around the line.
func TestAFullShareServesItsLineInOrder(t *testing.T) {
	l := newPermitLedger(3) // headroom 1
	first, err := tryAcquire(l, DialOrdinary)
	if err != nil {
		t.Fatal(err)
	}
	const waiters = 5
	order := make(chan int, waiters)
	var wg sync.WaitGroup
	for i := range waiters {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := l.acquire(dialMark{acquirer: ctx})
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			order <- i
			p.Release()
		}()
		// Join the line one at a time, so its order is the arrival order.
		waitFor(t, func() bool { return l.Snapshot().OrdinaryWaiting == i+1 })
	}
	// An arrival while the line is non-empty joins it rather than taking the
	// slot the release frees.
	first.Release()
	wg.Wait()
	close(order)
	want := 0
	for got := range order {
		if got != want {
			t.Fatalf("served waiter %d before waiter %d: the line is not first come first served", got, want)
		}
		want++
	}
}

// A CALLER THAT GIVES UP LEAVES THE LINE AND TAKES NOTHING. The slot it was
// waiting for goes to the next waiter, not to a dial nobody wants.
func TestAWaiterThatGivesUpLeavesTheLine(t *testing.T) {
	l := newPermitLedger(3) // headroom 1
	held, err := tryAcquire(l, DialOrdinary)
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.acquire(dialMark{acquirer: gone})
		done <- err
	}()
	waitFor(t, func() bool { return l.Snapshot().OrdinaryWaiting == 1 })
	cancel()
	if err := <-done; !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("a waiter that gave up got %v, want the budget refusal", err)
	}
	if n := l.Snapshot().OrdinaryWaiting; n != 0 {
		t.Fatalf("%d waiter(s) left in line after the caller gave up", n)
	}
	held.Release()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after the only holder released: the slot went to a waiter that had left", got)
	}
}

// A RAISE SERVES BOTH SHARES' LINES AT ONCE.
func TestARaiseServesWaitersAtOnce(t *testing.T) {
	l := newPermitLedger(3) // front door 1, headroom 1
	if _, err := tryAcquire(l, DialFrontDoor); err != nil {
		t.Fatal(err)
	}
	if _, err := tryAcquire(l, DialOrdinary); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan DialClass, 4)
	for _, class := range []DialClass{DialFrontDoor, DialFrontDoor, DialOrdinary} {
		go func() {
			if p, err := l.acquire(dialMark{intent: &dialIntent{class: class}, acquirer: ctx}); err == nil {
				got <- p.Class
			}
		}()
	}
	waitFor(t, func() bool {
		s := l.Snapshot()
		return s.FrontDoorWaiting == 2 && s.OrdinaryWaiting == 1
	})
	if err := l.SetBudget(10); err != nil { // front door 5, headroom 4
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("a raise left waiters in line beside free slots")
		}
	}
}

// A budget below the floor is refused, and a refused update changes nothing.
func TestLiveBudgetUpdatesAreValidated(t *testing.T) {
	l := newPermitLedger(10)
	for _, n := range []int{-1, 0, 1, 2} {
		if err := l.SetBudget(n); !errors.Is(err, ErrInvalidBudget) {
			t.Errorf("SetBudget(%d) = %v, want a refusal below meta.MinPoolConns (%d)", n, err, meta.MinPoolConns)
		}
	}
	if got := l.Snapshot().Configured; got != 10 {
		t.Errorf("a refused update changed the budget to %d", got)
	}
	if err := l.SetBudget(int(meta.MinPoolConns)); err != nil {
		t.Fatalf("the floor itself was refused: %v", err)
	}
}

// The reserved lane survives both shares being full, and a drain: a cancel is
// needed exactly when everything else is spent.
func TestTheControlLaneSurvivesSaturationAndADrain(t *testing.T) {
	l := newPermitLedger(10) // front door 5, headroom 4
	for range 4 {
		if _, err := tryAcquire(l, DialOrdinary); err != nil {
			t.Fatal(err)
		}
	}
	for range 5 {
		if _, err := tryAcquire(l, DialFrontDoor); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SetBudget(3); err != nil {
		t.Fatal(err)
	}
	if s := l.Snapshot(); !s.Draining {
		t.Fatalf("expected a draining ledger, got %+v", s)
	}
	rel, err := l.AcquireControl(context.Background())
	if err != nil {
		t.Fatalf("a cancellation at saturation, during a drain, was refused: %v", err)
	}
	// A second cancel WAITS for the lane on its own context rather than being lost.
	waiting, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.AcquireControl(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second cancel must wait on its own context, got %v", err)
	}
	rel.Release()
	if _, err := l.AcquireControl(context.Background()); err != nil {
		t.Fatalf("the lane must be reusable once released: %v", err)
	}
}

// A grant carries the terms it was made under, so an audit of a saturated or
// draining moment can say which policy admitted each live socket.
func TestAPermitCarriesItsGrantTerms(t *testing.T) {
	l := newPermitLedger(10) // front door 5, headroom 4
	ord, err := tryAcquire(l, DialOrdinary)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := tryAcquire(l, DialFrontDoor)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := l.AcquireControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p     *Permit
		class DialClass
		limit int
	}{{ord, DialOrdinary, 4}, {fd, DialFrontDoor, 5}, {ctl, DialControl, 1}} {
		if c.p.Class != c.class || c.p.Limit != c.limit || c.p.ConfiguredTotal != 10 {
			t.Errorf("%v grant: class %v, limit %d, configured %d; want %v, %d, 10",
				c.class, c.p.Class, c.p.Limit, c.p.ConfiguredTotal, c.class, c.limit)
		}
	}
	gen := ord.Generation
	if err := l.SetBudget(3); err != nil {
		t.Fatal(err)
	}
	if ord.Generation != gen {
		t.Errorf("a live permit's generation changed under it: %d -> %d", gen, ord.Generation)
	}
	ord.Release()
	fd.Release()
	ctl.Release()
	if got := l.Outstanding(); got != 0 {
		t.Errorf("outstanding = %d after releases, want 0", got)
	}
}

// A connection outside every pool has no ledger, and its dialer is the
// underlying one, unwrapped.
func TestAConnectionOutsideEveryPoolHasNoLedger(t *testing.T) {
	e := New(nil, nil)
	e.pools.setPoolOf(&meta.Connection{ID: 9, Engine: engine.MySQL, PoolID: meta.DefaultPoolID})
	if l := e.pools.ledgerFor(9); l != nil {
		t.Fatal("a MySQL connection was given a pool ledger; it is outside every pool whatever its pool_id says")
	}
	if !e.reusesBackend(9) {
		t.Error("a connection outside every pool must keep backend reuse")
	}
	var dialed int
	dial := permitDialer(nil, func(context.Context, string, string) (net.Conn, error) {
		dialed++
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return client, nil
	})
	for range 10 {
		c, err := dial(context.Background(), "tcp", "a")
		if err != nil {
			t.Fatalf("an unbudgeted dial was refused: %v", err)
		}
		_ = c.Close()
	}
	if dialed != 10 {
		t.Errorf("dialed %d times, want 10", dialed)
	}
}

// The dialer's three obligations: take a permit before the dial, release it
// when the socket closes, and release it if the dial fails.
func TestPermitDialerReleasesOnDialFailure(t *testing.T) {
	l := newPermitLedger(3)
	boom := errors.New("connection refused")
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		return nil, boom
	})
	for i := range 5 {
		if _, err := dial(context.Background(), "tcp", "10.0.0.1:5432"); !errors.Is(err, boom) {
			t.Fatalf("dial %d: err = %v, want the dial error", i, err)
		}
		if got := l.Outstanding(); got != 0 {
			t.Fatalf("after failed dial %d outstanding = %d, want 0 — a failed dial leaked a permit", i, got)
		}
	}
}

func TestPermitDialerReleasesWhenTheSocketCloses(t *testing.T) {
	l := newPermitLedger(3) // headroom 1
	server, client := net.Pipe()
	defer server.Close()
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		return client, nil
	})
	conn, err := dial(context.Background(), "tcp", "10.0.0.1:5432")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Outstanding(); got != 1 {
		t.Fatalf("outstanding = %d, want 1 while the socket is open", got)
	}
	if _, err := dial(doneCtx(), "tcp", "10.0.0.1:5432"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("second dial: err = %v, want the budget refusal", err)
	}
	_ = conn.Close()
	_ = conn.Close()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after close (twice), want 0", got)
	}
}

// A refused dial must not open a socket at all.
func TestPermitDialerDoesNotDialWhenTheShareIsSpent(t *testing.T) {
	l := newPermitLedger(3) // headroom 1
	var dialed int
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		dialed++
		server, client := net.Pipe()
		go server.Close()
		return client, nil
	})
	if _, err := dial(context.Background(), "tcp", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(doneCtx(), "tcp", "b"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	if dialed != 1 {
		t.Errorf("dialed %d times, want 1 — the refusal must happen BEFORE the dial", dialed)
	}
}

// ONLY A MARKED DIAL TAKES ANOTHER SHARE OR THE LANE, and the marks can only
// be set inside this package. An unmarked dial is ordinary work.
func TestOnlyAMarkedDialTakesAnotherShareOrTheLane(t *testing.T) {
	l := newPermitLedger(3) // front door 1, headroom 1, lane 1
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		_, client := net.Pipe()
		return client, nil
	})
	if _, err := dial(context.Background(), "tcp", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(doneCtx(), "tcp", "b"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("an unmarked dial must never take the front-door share or the lane")
	}
	intent := &dialIntent{class: DialFrontDoor}
	if _, err := dial(withDialIntent(doneCtx(), intent), "tcp", "pin"); err != nil {
		t.Fatalf("a front-door pin must have its share: %v", err)
	}
	if !intent.granted.Load() {
		t.Error("the pin's intent must record that its permit was granted")
	}
	if _, err := dial(withDialIntent(context.Background(), &dialIntent{class: DialControl}), "tcp", "cancel"); err != nil {
		t.Fatalf("a cancellation at full budget must still be delivered: %v", err)
	}
}

// waitFor polls a condition the cell can't be told about directly, failing
// after a bound rather than hanging the suite.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// The deprecated debug flag selects nothing: both states get the one common
// bound, whatever the deprecated value says.
func TestDebugFlagSelectsNothing(t *testing.T) {
	base := txLimits{idleInTx: 2 * time.Hour, maxTx: 8 * time.Hour}

	for _, tc := range []struct {
		name      string
		debug     bool
		debugIdle time.Duration
	}{
		{"not debug", false, 0},
		{"debug, shorter deprecated value", true, 10 * time.Minute},
		{"debug, longer deprecated value", true, 4 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base.forConnection(tc.debug, tc.debugIdle, 8*time.Hour)
			if got.idleInTx != 2*time.Hour {
				t.Errorf("idleInTx = %v, want the one common 2h bound — the deprecated flag "+
					"must not select a different behaviour", got.idleInTx)
			}
		})
	}
}

// The ceiling must not silently clip the ruled maximum.
func TestTheCeilingDoesNotClipTheRuledMaximum(t *testing.T) {
	l := txLimits{idleInTx: DefaultIdleInTxTimeout, maxTx: DefaultMaxTxDuration}
	got := l.forConnection(false, 0, DefaultMaxTxDurationCeiling)
	if got.maxTx != DefaultMaxTxDuration {
		t.Errorf("effective maxTx = %v, want %v — the ceiling clipped the ruled policy, "+
			"which is the defect where a displayed 8h becomes a 30m runtime",
			got.maxTx, DefaultMaxTxDuration)
	}
}

// The core/exec aliases of the policy defaults, pinned to literals.
//
// core/config's aliases are pinned the same way. Literal on purpose: an alias
// pointed at the wrong core/timeouts constant agrees with it perfectly.
func TestExecPolicyMirrorIsTheRuledValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"session idle", DefaultSessionIdleTimeout, 10 * time.Minute},
		{"idle in transaction", DefaultIdleInTxTimeout, 30 * time.Minute},
		{"max transaction duration", DefaultMaxTxDuration, 2 * time.Hour},
		{"max transaction ceiling", DefaultMaxTxDurationCeiling, 2 * time.Hour},
		{"deprecated debug idle", DefaultDebugIdleInTxTimeout, 30 * time.Minute},
		{"pool idle", DefaultPoolMaxConnIdleTime, 10 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v — the core/exec alias is not the ruled policy",
				tc.name, tc.got, tc.want)
		}
	}
}
