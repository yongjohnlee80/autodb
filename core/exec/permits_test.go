package exec

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPermitLedgerBoundsOutstandingSockets(t *testing.T) {
	l := newPermitLedger(3)
	var releases []func()
	for i := range 3 {
		rel, err := l.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, rel)
	}
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("the fourth acquire must be refused, got %v", err)
	}
	releases[0]()
	if _, err := l.Acquire(); err != nil {
		t.Fatalf("a released permit must be reusable: %v", err)
	}
}

// Releasing twice must not manufacture a slot that does not exist.
func TestReleaseIsIdempotent(t *testing.T) {
	l := newPermitLedger(1)
	rel, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel()
	rel()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d, want 0 — a double release invented capacity", got)
	}
	if _, err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("budget of 1 must still be 1 after repeated releases")
	}
}

// ADR 0181: lowering the budget must DRAIN, never kill. This is the 50 -> 25
// case with 50 sockets already open.
func TestLoweringTheBudgetDrainsRatherThanKilling(t *testing.T) {
	l := newPermitLedger(50)
	var releases []func()
	for range 50 {
		rel, err := l.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}

	_, genBefore := l.Budget()
	l.SetBudget(25)
	budget, genAfter := l.Budget()
	if budget != 25 {
		t.Fatalf("budget = %d, want 25", budget)
	}
	if genAfter == genBefore {
		t.Error("a budget change must open a new generation; monotonicity is per-generation")
	}

	// Nothing was closed: the fifty live sockets are still live.
	if got := l.Outstanding(); got != 50 {
		t.Fatalf("outstanding = %d, want 50 — lowering the budget must not kill live work", got)
	}
	// And no new dial is granted while over the new budget.
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("no new permit may be granted above the lowered budget")
	}
	// Draining below the new budget makes room again.
	for i := range 26 {
		releases[i]()
	}
	if got := l.Outstanding(); got != 24 {
		t.Fatalf("outstanding = %d, want 24", got)
	}
	if _, err := l.Acquire(); err != nil {
		t.Fatalf("once drained below the budget a dial must be granted: %v", err)
	}
}

// The ledger is the aggregate across targets, so concurrent acquirers must
// never collectively exceed it.
func TestConcurrentAcquiresNeverExceedTheBudget(t *testing.T) {
	const budget = 8
	l := newPermitLedger(budget)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		peak    int
		held    []func()
	)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire()
			if err != nil {
				return
			}
			mu.Lock()
			granted++
			held = append(held, rel)
			if granted > peak {
				peak = granted
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > budget {
		t.Fatalf("peak outstanding = %d, budget = %d — the ledger over-granted", peak, budget)
	}
	if l.Outstanding() != len(held) {
		t.Fatalf("outstanding = %d, granted-and-held = %d", l.Outstanding(), len(held))
	}
}

// A budget of zero means unbounded, matching how the engine treats an unset
// pool cap. ADR 0181 D3 requires the CONFIG to have no default; that is a
// validation rule at load, not a reason for the ledger to invent a limit.
func TestZeroBudgetIsUnbounded(t *testing.T) {
	l := newPermitLedger(0)
	for i := range 100 {
		if _, err := l.Acquire(); err != nil {
			t.Fatalf("acquire %d under an unset budget: %v", i, err)
		}
	}
}

// The dialer is where the ADR's three obligations meet: take a permit before
// the dial, release it when the socket closes, and release it if the dial
// fails.
func TestPermitDialerReleasesOnDialFailure(t *testing.T) {
	l := newPermitLedger(1)
	boom := errors.New("connection refused")
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		return nil, boom
	})

	for i := range 5 {
		if _, err := dial(context.Background(), "tcp", "10.0.0.1:5432"); !errors.Is(err, boom) {
			t.Fatalf("dial %d: err = %v, want the dial error", i, err)
		}
		if got := l.Outstanding(); got != 0 {
			t.Fatalf("after failed dial %d outstanding = %d, want 0 — a failed dial leaked a permit, "+
				"and a leaked permit is a slot lost until restart", i, got)
		}
	}
}

func TestPermitDialerReleasesWhenTheSocketCloses(t *testing.T) {
	l := newPermitLedger(1)
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
	// The budget is spent: a second dial is refused.
	if _, err := dial(context.Background(), "tcp", "10.0.0.1:5432"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("second dial: err = %v, want the budget refusal", err)
	}

	_ = conn.Close()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after close, want 0", got)
	}
	// And closing again must not invent capacity.
	_ = conn.Close()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after a second close, want 0", got)
	}
}

// A refused dial must not open a socket at all.
func TestPermitDialerDoesNotDialWhenTheBudgetIsSpent(t *testing.T) {
	l := newPermitLedger(0)
	l.SetBudget(1)
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
	if _, err := dial(context.Background(), "tcp", "b"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	if dialed != 1 {
		t.Errorf("dialed %d times, want 1 — the refusal must happen BEFORE the dial", dialed)
	}
}

// The three copies of the transaction bounds must agree.
//
// They live in core/config, in this package's mirror, and in defaultTxLimits.
// engine.go builds every Engine from the last one, so a change to the first
// two alone is a NO-OP that the rest of the suite would report as green --
// which is exactly why this asserts the identity rather than the values.
func TestDefaultTxLimitsMatchTheDeclaredConstants(t *testing.T) {
	l := defaultTxLimits()
	if l.idleInTx != DefaultIdleInTxTimeout {
		t.Errorf("defaultTxLimits idleInTx = %v, constant = %v — the copies have drifted",
			l.idleInTx, DefaultIdleInTxTimeout)
	}
	if l.maxTx != DefaultMaxTxDuration {
		t.Errorf("defaultTxLimits maxTx = %v, constant = %v — the copies have drifted",
			l.maxTx, DefaultMaxTxDuration)
	}
}

// The deprecated debug flag may lengthen the idle bound, never shorten it.
func TestDebugProfileCannotShortenTheIdleBound(t *testing.T) {
	base := txLimits{idleInTx: 2 * time.Hour, maxTx: 8 * time.Hour}

	shorter := base.forConnection(true, 10*time.Minute, 8*time.Hour)
	if shorter.idleInTx != 2*time.Hour {
		t.Errorf("a debug connection got %v, want the base 2h — the flag must never "+
			"hand a debugging session LESS tolerance than an ordinary one", shorter.idleInTx)
	}

	longer := base.forConnection(true, 4*time.Hour, 8*time.Hour)
	if longer.idleInTx != 4*time.Hour {
		t.Errorf("a longer debug bound was not applied: got %v, want 4h", longer.idleInTx)
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
