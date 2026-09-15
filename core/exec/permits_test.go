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
	// A budget of 4 leaves 3 for ordinary dials; the fourth is the reserved
	// control lane.
	l := newPermitLedger(4)
	var releases []func()
	for i := range 3 {
		rel, err := l.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, rel.Release)
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
	l := newPermitLedger(2) // 1 ordinary + 1 reserved
	p, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	p.Release()
	p.Release()
	if got := l.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d, want 0 — a double release invented capacity", got)
	}
	if _, err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("the ordinary allowance must be unchanged after repeated releases")
	}
}

// the connection-budget policy: lowering the budget must DRAIN, never kill. This is the 50 -> 25
// case with 50 sockets already open.
func TestLoweringTheBudgetDrainsRatherThanKilling(t *testing.T) {
	l := newPermitLedger(51) // 50 ordinary + 1 reserved
	var releases []func()
	for range 50 {
		rel, err := l.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel.Release)
	}

	_, genBefore := l.Budget()
	if err := l.SetBudget(26); err != nil { // 25 ordinary + 1 reserved
		t.Fatal(err)
	}
	budget, genAfter := l.Budget()
	if budget != 26 {
		t.Fatalf("budget = %d, want 26", budget)
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
	const budget = 8 // 7 ordinary + 1 reserved
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
			held = append(held, rel.Release)
			if granted > peak {
				peak = granted
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > budget-1 {
		t.Fatalf("peak ordinary outstanding = %d, ordinary allowance = %d — the ledger over-granted",
			peak, budget-1)
	}
	if l.Outstanding() != len(held) {
		t.Fatalf("outstanding = %d, granted-and-held = %d", l.Outstanding(), len(held))
	}
}

// A budget of zero means unbounded, matching how the engine treats an unset
// pool cap. the no-default rule requires the CONFIG to have no default; that is a
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
	l := newPermitLedger(2)
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
	l := newPermitLedger(2) // 1 ordinary + 1 reserved
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
	if err := l.SetBudget(2); err != nil { // 1 ordinary + 1 reserved
		t.Fatal(err)
	}
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

// The reserved lane exists so a cancellation can still be delivered when every
// ordinary slot is spent — which is exactly when someone reaches for cancel.
func TestTheControlLaneSurvivesOrdinarySaturation(t *testing.T) {
	l := newPermitLedger(3) // 2 ordinary + 1 reserved

	for i := range 2 {
		if _, err := l.Acquire(); err != nil {
			t.Fatalf("ordinary acquire %d: %v", i, err)
		}
	}
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("ordinary dials must stop one short of the budget")
	}

	rel, err := l.AcquireControl(context.Background())
	if err != nil {
		t.Fatalf("the control lane must be available at ordinary saturation: %v", err)
	}
	// Serialized: one at a time is enough for connect-write-close, and more
	// would be a second budget nobody configured.
	// A second cancel WAITS for the lane rather than being lost: the caller has
	// already given up on their query and has no way to learn the cancel never
	// went.
	waiting, cancelWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWait()
	if _, err := l.AcquireControl(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second cancel must WAIT on its own context, got %v", err)
	}
	rel.Release()
	if _, err := l.AcquireControl(context.Background()); err != nil {
		t.Fatalf("the lane must be reusable once released: %v", err)
	}
}

// The reserved lane is only reachable through the marker, and the marker is
// only settable inside this package.
func TestOnlyAMarkedDialTakesTheControlLane(t *testing.T) {
	l := newPermitLedger(2) // 1 ordinary + 1 reserved
	dial := permitDialer(l, func(context.Context, string, string) (net.Conn, error) {
		_, client := net.Pipe()
		return client, nil
	})

	// Saturate the ordinary allowance.
	if _, err := dial(context.Background(), "tcp", "a"); err != nil {
		t.Fatal(err)
	}
	// An UNMARKED dial cannot help itself to the reserved slot.
	if _, err := dial(context.Background(), "tcp", "b"); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("an ordinary dial must never consume the reserved lane")
	}
	// A MARKED dial gets it — this is a cancellation at saturation, which is
	// the only moment cancellation matters.
	if _, err := dial(withControlDial(context.Background()), "tcp", "cancel"); err != nil {
		t.Fatalf("a cancellation at full budget must still be delivered: %v", err)
	}
}

// The core/exec mirror of the policy defaults, pinned to literals.
//
// core/config carries the same numbers and pins them independently. Both are
// literal on purpose: comparing one copy to the other passes when BOTH are
// wrong, which is the failure mode a mirror invites.
func TestExecPolicyMirrorIsTheRuledValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"session idle", DefaultSessionIdleTimeout, 10 * time.Minute},
		{"idle in transaction", DefaultIdleInTxTimeout, 2 * time.Hour},
		{"max transaction duration", DefaultMaxTxDuration, 8 * time.Hour},
		{"max transaction ceiling", DefaultMaxTxDurationCeiling, 8 * time.Hour},
		{"deprecated debug idle", DefaultDebugIdleInTxTimeout, 2 * time.Hour},
		{"pool idle", DefaultPoolMaxConnIdleTime, 10 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v — the core/exec mirror has drifted from the ruled policy",
				tc.name, tc.got, tc.want)
		}
	}
}

// A live update must not be able to remove a bound that configuration refuses
// to remove at startup.
func TestLiveBudgetUpdatesAreValidated(t *testing.T) {
	l := newPermitLedger(10)
	for _, n := range []int{0, -1, 1} {
		if err := l.SetBudget(n); !errors.Is(err, ErrInvalidBudget) {
			t.Errorf("SetBudget(%d) = %v, want a refusal — a nonpositive budget is not "+
				"'unlimited', and 1 is consumed entirely by the reserved lane", n, err)
		}
	}
	if got := l.Snapshot().Configured; got != 10 {
		t.Errorf("a refused update changed the budget to %d", got)
	}
	if err := l.SetBudget(4); err != nil {
		t.Fatalf("a valid update was refused: %v", err)
	}
}

// The snapshot's fields only mean something together, so they are read under
// one lock — and it must say plainly when a lowered budget is draining rather
// than leaving a caller to compare two numbers and get it backwards.
func TestSnapshotIsCoherentAndNamesTheDrain(t *testing.T) {
	l := newPermitLedger(6) // 5 ordinary + 1 reserved

	before := l.Snapshot()
	if before.Configured != 6 || before.OrdinaryLimit != 5 {
		t.Fatalf("configured=%d ordinary=%d, want 6 and 5", before.Configured, before.OrdinaryLimit)
	}
	if before.Draining {
		t.Error("an untouched ledger is not draining")
	}

	for range 5 {
		if _, err := l.Acquire(); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SetBudget(3); err != nil {
		t.Fatal(err)
	}

	after := l.Snapshot()
	if after.Generation <= before.Generation {
		t.Error("an accepted update must open a new generation")
	}
	if after.Outstanding != 5 {
		t.Errorf("outstanding = %d, want 5 — lowering the budget must not kill live work",
			after.Outstanding)
	}
	if !after.Draining {
		t.Error("outstanding above configured IS the drain; the snapshot must say so rather " +
			"than leaving a caller to derive it")
	}
}

// THE DRAIN CASE. While a lowered budget drains, outstanding is above the new
// number by definition — so a control lane that re-tested against it would
// refuse a cancellation precisely because too much work is already running,
// which is the one moment cancelling matters most.
func TestTheControlLaneSurvivesADrainingGeneration(t *testing.T) {
	l := newPermitLedger(51) // 50 ordinary + 1 reserved
	for range 50 {
		if _, err := l.Acquire(); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SetBudget(26); err != nil { // 25 ordinary + 1 reserved
		t.Fatal(err)
	}

	snap := l.Snapshot()
	if !snap.Draining || snap.Outstanding <= snap.Configured {
		t.Fatalf("expected a draining ledger, got %+v", snap)
	}
	// Ordinary work is correctly refused while over the new budget...
	if _, err := l.Acquire(); !errors.Is(err, ErrTargetBudgetExhausted) {
		t.Fatal("no new ordinary permit may be granted above the lowered budget")
	}
	// ...and a cancellation still goes through.
	if _, err := l.AcquireControl(context.Background()); err != nil {
		t.Fatalf("a cancellation during a drain was refused: %v — this is the state where "+
			"cancelling matters most", err)
	}
}

// A grant carries the terms it was made under, so an audit of a saturated or
// draining moment can say which policy admitted each live socket.
func TestAPermitCarriesItsGrantTerms(t *testing.T) {
	l := newPermitLedger(6) // 5 ordinary + 1 reserved

	ord, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if ord.Class != DialOrdinary {
		t.Errorf("class = %v, want ordinary", ord.Class)
	}
	if ord.Limit != 5 {
		t.Errorf("ordinary limit = %d, want 5 — one short of the budget", ord.Limit)
	}
	genAtGrant := ord.Generation

	ctl, err := l.AcquireControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ctl.Class != DialControl {
		t.Errorf("class = %v, want control", ctl.Class)
	}
	if ctl.Limit != 6 {
		t.Errorf("control limit = %d, want the full budget 6", ctl.Limit)
	}

	// A permit granted before an update keeps the generation that admitted it:
	// that correlation is the whole reason the field exists.
	if err := l.SetBudget(3); err != nil {
		t.Fatal(err)
	}
	if ord.Generation != genAtGrant {
		t.Errorf("a live permit's generation changed under it: %d -> %d",
			genAtGrant, ord.Generation)
	}
	if now := l.Snapshot().Generation; now == genAtGrant {
		t.Error("the ledger's generation did not advance on an accepted update")
	}

	ord.Release()
	ord.Release()
	ctl.Release()
	if got := l.Outstanding(); got != 0 {
		t.Errorf("outstanding = %d after releases, want 0", got)
	}
}

// The EXPORTED engine path, across the transitions an operator actually makes.
func TestEngineTargetBudgetTransitions(t *testing.T) {
	// Built through the real constructor: Settings() reads session state a bare
	// struct does not have, and the point of this cell is the EXPORTED path.
	e := New(nil, nil, WithTargetConnBudget(51)) // 50 ordinary + 1 reserved
	var held []*Permit
	for range 50 {
		p, err := e.targetPermits.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
	}

	// 50 -> 25: drains, kills nothing.
	if err := e.SetTargetConnBudget(26); err != nil {
		t.Fatalf("lowering the budget was refused: %v", err)
	}
	s := e.Settings().TargetConns
	if s.Outstanding != 50 || !s.Draining {
		t.Fatalf("after 50->25: outstanding=%d draining=%v, want 50 and true",
			s.Outstanding, s.Draining)
	}
	if s.Effective != 50 {
		t.Errorf("effective = %d, want 50 — during a drain the configured number is NOT "+
			"the number of sockets that exist, and an operator sizing a server needs "+
			"the one that is true", s.Effective)
	}

	// A refused update must change nothing, including the generation.
	genBefore := e.Settings().TargetConns.Generation
	if err := e.SetTargetConnBudget(0); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("a nonpositive update was accepted: %v", err)
	}
	after := e.Settings().TargetConns
	if after.Generation != genBefore {
		t.Errorf("a REFUSED update advanced the generation %d -> %d; a reader comparing "+
			"snapshots would believe the policy changed", genBefore, after.Generation)
	}
	if after.Configured != 26 {
		t.Errorf("a refused update changed the budget to %d", after.Configured)
	}

	// 25 -> 60: raising ends the drain immediately, without touching sockets.
	if err := e.SetTargetConnBudget(61); err != nil {
		t.Fatal(err)
	}
	if up := e.Settings().TargetConns; up.Draining || up.Effective != 61 {
		t.Errorf("after raising: draining=%v effective=%d, want false and 61",
			up.Draining, up.Effective)
	}

	// 25 -> 20: lowering again while still over.
	if err := e.SetTargetConnBudget(21); err != nil {
		t.Fatal(err)
	}
	if down := e.Settings().TargetConns; !down.Draining || down.Effective != 50 {
		t.Errorf("after lowering again: draining=%v effective=%d, want true and 50",
			down.Draining, down.Effective)
	}

	// Draining below the new budget clears the state.
	for i := range 35 {
		held[i].Release()
	}
	if end := e.Settings().TargetConns; end.Draining || end.Outstanding != 15 {
		t.Errorf("after draining: draining=%v outstanding=%d, want false and 15",
			end.Draining, end.Outstanding)
	}
}

// An engine built without a budget refuses a live update rather than silently
// creating one nobody configured.
func TestEngineWithoutABudgetRefusesLiveUpdates(t *testing.T) {
	e := New(nil, nil)
	if err := e.SetTargetConnBudget(25); !errors.Is(err, ErrInvalidBudget) {
		t.Errorf("err = %v, want a refusal", err)
	}
	if got := e.Settings().TargetConns; got.Configured != 0 {
		t.Errorf("an engine without a budget reported %d", got.Configured)
	}
}
