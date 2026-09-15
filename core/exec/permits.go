package exec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrTargetBudgetExhausted means this instance already holds every production
// connection its operator allowed it to hold.
//
// It is a CAPACITY refusal and never charges the credential throttle — see
// ChargeCapacity and the charge-class policy.
var ErrTargetBudgetExhausted = errors.New("exec: target connection budget exhausted")

// ErrInvalidBudget means a live budget update was refused.
var ErrInvalidBudget = errors.New("exec: invalid target connection budget")

// DialClass says which side of the budget a socket was charged to.
type DialClass uint8

const (
	// DialOrdinary is query, health-check and control-query traffic: it stops
	// one short of the budget.
	DialOrdinary DialClass = iota
	// DialControl is the reserved lane — today, a cancellation.
	DialControl
)

func (c DialClass) String() string {
	if c == DialControl {
		return "control"
	}
	return "ordinary"
}

// Permit is one granted socket allowance.
//
// IT CARRIES THE TERMS IT WAS GRANTED UNDER, not just a release. A permit
// taken at a budget of 50 and released during a drain to 25 is correlated to
// the generation that admitted it -- without that, an audit of a saturated or
// draining moment cannot tell which policy each live socket was granted by,
// and "outstanding is above the budget" looks like a bug rather than the drain
// it is.
type Permit struct {
	// Generation is the ledger generation that granted it.
	Generation uint64
	// Limit is the allowance in force for this permit's class at grant time.
	Limit int
	// Class is which side of the budget it was charged to.
	Class DialClass
	// ConfiguredTotal and EffectiveCeiling are the operator's number and the
	// immediately exercisable ceiling at grant time. Limit alone is ambiguous
	// during a drain: a control permit's limit is 1 either way, which says
	// nothing about the policy it was admitted under.
	ConfiguredTotal  int
	EffectiveCeiling int

	once    sync.Once
	release func()
}

// Release returns the permit. It is IDEMPOTENT: a permit released twice hands
// out a slot that does not exist, one never released is a slot lost until
// restart, and both are silent -- so the safe shape is one that can be called
// from every unwind path without the caller tracking whether it already ran.
func (p *Permit) Release() {
	if p == nil {
		return
	}
	p.once.Do(p.release)
}

// permitLedger enforces `exec.max_target_conns`: the total number of sockets
// this instance may have open to production targets, across every target.
//
// A RUNTIME PERMIT, NOT A SUM OF CONFIGURED CAPS (The policy). Summing each
// target's pool_max_conns pre-slices the budget: two targets configured at 8
// each reserve 16 whether or not either is busy, so one target queues while
// the other's slice sits idle and nothing can rebalance. A permit is taken
// when a socket is about to be dialled and released when it closes, so the
// budget is spent by connections that actually exist.
//
// Per-target pool_max_conns remains a technical CEILING on one pool. It is
// not an allocation, and two targets may each be allowed more than the budget
// — the ledger is what makes that safe (see docs/front-door/connection-holding-policy.md).
type permitLedger struct {
	mu     sync.Mutex
	budget int
	// ordinary (O) and control (K) are counted SEPARATELY, and that is not
	// bookkeeping taste. With one combined counter the effective ceiling rose
	// and fell inside a single drain generation as the short-lived cancel
	// socket came and went -- 49, then 50, then 49 -- which contradicts the
	// rule that a drain converges monotonically. Separating them lets the
	// ceiling be stated in terms of ordinary occupancy alone.
	ordinary int
	control  int
	// generation increments on every budget change. It exists because a
	// lowered budget cannot be enforced retroactively: with 50 sockets open
	// and a new budget of 25, the invariant "outstanding <= budget" is FALSE
	// and stays false until sockets close on their own. Monotonicity is
	// therefore asserted per generation rather than absolutely, and a caller
	// that wants to reason about a drain needs to know which generation it is
	// looking at.
	generation uint64
	// controlLane serializes the reserved slot to one holder.
	controlLane chan struct{}
}

// newPermitLedger builds a ledger for exactly the budget it is given.
//
// IT NEVER RAISES A BUDGET. An earlier version silently turned anything below
// 2 into 2, which let a caller authorising ONE socket end up holding two --
// the ledger claiming more of a production database than it was permitted,
// quietly, which is the precise failure this whole mechanism exists to
// prevent. A number the caller did not choose is not a safe default just
// because it is small.
//
// A budget of 1 is therefore honoured as 1: the single slot is the control
// lane's, the ordinary limit is 0, and every ordinary dial is refused. That is
// a useless configuration, and it is REFUSED WHERE CONFIGURATION IS VALIDATED
// rather than corrected here -- an operator gets a message naming the key, not
// a daemon that quietly does something else.
//
// THERE IS NO "UNBOUNDED" LEDGER. A zero budget used to mean unlimited, which
// was dead surface and incoherent besides: under the O/K algebra it reported
// Effective = O+1 and Draining = true the moment anything was acquired.
// ABSENCE IS A NIL LEDGER, which is what production already does -- an engine
// built without a budget has no ledger at all and the dialer returns the
// underlying dial untouched.
func newPermitLedger(budget int) *permitLedger {
	return &permitLedger{budget: budget, generation: 1, controlLane: make(chan struct{}, 1)}
}

// SetBudget publishes a new budget as a new generation.
//
// IT NEVER CLOSES ANYTHING. Lowering the budget from 50 to 25 while 50 sockets
// are open must not kill twenty-five live sessions to make the number true —
// that would take a configuration edit and turn it into an outage. Instead no
// new permit is granted above the new budget and outstanding DRAINS as work
// finishes. The number becomes true by attrition, which is the only way it can
// become true without breaking correct work.
func (l *permitLedger) SetBudget(n int) error {
	// A NONPOSITIVE BUDGET IS NOT "UNLIMITED", and accepting it here would make
	// a live update the one way to remove a production-safety bound that
	// configuration refuses to remove at startup.
	if n < 2 {
		return fmt.Errorf("%w: a live budget update to %d is refused; the minimum is 2, "+
			"because one permit is reserved so a cancellation can still be delivered",
			ErrInvalidBudget, n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budget = n
	l.generation++
	return nil
}

// LedgerSnapshot is one coherent reading of the ledger.
//
// TAKEN UNDER ONE LOCK, because the fields only mean something together.
// Reading outstanding and then budget separately can observe a state that
// never existed — an outstanding count from before a resize against a budget
// from after it — and an operator diagnosing saturation would be looking at a
// number no moment ever held.
type LedgerSnapshot struct {
	// Configured is the operator's number.
	Configured int
	// OrdinaryLimit is what ordinary work may actually take: one short of
	// Configured, because the last permit is the control lane's.
	OrdinaryLimit int
	// Ordinary (O) and Control (K) are the two occupancies, reported
	// separately because the ceiling is stated in terms of O alone.
	Ordinary int
	Control  int
	// Outstanding is O + K: every socket this instance holds.
	//
	// It may EXCEED Configured while a lowered budget drains. That is
	// not an error and must not be rendered as one: nothing is killed to make
	// the number true, so it becomes true by attrition.
	Outstanding int
	// Effective is the immediately exercisable ceiling: max(Configured, O+1).
	//
	// O+1, not Outstanding. The reserved slot counts whether or not a cancel
	// is in flight, because one may arrive at any moment -- and counting it
	// only while occupied made this number rise and fall INSIDE one drain
	// generation, contradicting monotonic convergence.
	Effective int
	// Generation increments on every accepted update, so a reader can tell
	// whether two snapshots describe the same policy.
	Generation uint64
	// Draining reports the state above plainly, so a caller does not have to
	// re-derive it and get the comparison backwards.
	Draining bool
}

// Snapshot returns a coherent reading of the ledger.
func (l *permitLedger) Snapshot() LedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LedgerSnapshot{
		Configured:    l.budget,
		OrdinaryLimit: l.ordinaryLimitLocked(),
		Ordinary:      l.ordinary,
		Control:       l.control,
		Outstanding:   l.ordinary + l.control,
		Effective:     l.effectiveLocked(),
		Generation:    l.generation,
		Draining:      l.ordinary > l.ordinaryLimitLocked(),
	}
}

// Acquire takes one permit, or reports that the budget is spent.
//
// The returned release is IDEMPOTENT. A permit released twice would hand out a
// slot that does not exist; one never released is a slot lost for the life of
// the process. Both are silent, so the safe shape is a release that can be
// called from every unwind path — dial failure, cancellation, close — without
// the caller tracking whether it already ran.
func (l *permitLedger) Acquire() (*Permit, error) {
	return l.acquire(DialOrdinary)
}

// AcquireControl takes the RESERVED slot: the one an ordinary dial can never
// have.
//
// A cancellation is a second, short-lived socket to the same server, and it is
// needed exactly when every ordinary slot is spent — a developer cancels a
// query because the system is busy, not because it is idle. Letting ordinary
// traffic consume the whole budget therefore removes cancellation at the only
// moment anyone reaches for it.
//
// Serialized to one at a time: a cancel is connect, write sixteen bytes,
// close, so one lane is enough, and more would be a second budget nobody
// configured.
// It WAITS for the lane rather than refusing it. Two statements cancelled at
// once is ordinary, and an immediate refusal would silently lose the second
// cancel — the caller has already given up on their query and has no way to
// learn the cancel never went. The wait is bounded by the caller's own
// context, which pgx already gives a deadline.
func (l *permitLedger) AcquireControl(ctx context.Context) (*Permit, error) {
	select {
	case l.controlLane <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p, err := l.acquire(DialControl)
	if err != nil {
		<-l.controlLane
		return nil, err
	}
	inner := p.release
	p.release = func() {
		inner()
		<-l.controlLane
	}
	return p, nil
}

func (l *permitLedger) acquire(class DialClass) (*Permit, error) {
	control := class == DialControl
	l.mu.Lock()
	// ORDINARY DIALS STOP ONE SHORT. The last permit is the control lane's,
	// so a cancellation can always be delivered.
	//
	// THE CONTROL LANE DOES NOT RE-TEST AGAINST outstanding, and that is
	// deliberate. Its bound is the semaphore above — one holder, short-lived —
	// and re-testing here would break it in the one state where cancelling
	// matters most: while a LOWERED budget drains, outstanding is above the
	// new number by definition, so a cancel would be refused precisely because
	// too much work is already running. The reserved slot is a fixed +1 the
	// operator's number accounts for, stated here rather than hidden.
	if !control {
		limit := l.ordinaryLimitLocked()
		if l.ordinary >= limit {
			l.mu.Unlock()
			return nil, ErrTargetBudgetExhausted
		}
	}
	if control {
		l.control++
	} else {
		l.ordinary++
	}
	granted := Permit{
		Generation:       l.generation,
		Class:            class,
		ConfiguredTotal:  l.budget,
		EffectiveCeiling: l.effectiveLocked(),
		Limit:            l.limitForLocked(class),
	}
	l.mu.Unlock()

	granted.release = func() {
		l.mu.Lock()
		if control {
			if l.control > 0 {
				l.control--
			}
		} else if l.ordinary > 0 {
			l.ordinary--
		}
		l.mu.Unlock()
	}
	return &granted, nil
}

// ordinaryLimitLocked is what ordinary work may take: one short of the budget,
// because the last slot is the control lane's. Caller holds the lock.
func (l *permitLedger) ordinaryLimitLocked() int {
	// max(C-1, 0). At C=1 the single slot belongs to the control lane and
	// ordinary work gets nothing -- which is what C=1 MEANS, rather than
	// something to round away.
	if l.budget < 1 {
		return 0
	}
	return l.budget - 1
}

// effectiveLocked is the immediately exercisable ceiling: max(Configured,
// O+1).
//
// O+1 RATHER THAN Outstanding, and the +1 applies whether or not the lane is
// occupied. The reserved slot is part of the ceiling at all times -- a cancel
// may arrive at any moment -- so counting it only while a cancel is in flight
// made the number oscillate within one drain generation and contradicted the
// monotonic-convergence rule. Caller holds the lock.
func (l *permitLedger) effectiveLocked() int {
	if l.ordinary+1 > l.budget {
		return l.ordinary + 1
	}
	return l.budget
}

// limitForLocked is the allowance in force for a class at grant time.
func (l *permitLedger) limitForLocked(class DialClass) int {
	if class == DialControl {
		return 1 // the lane is one holder, whatever the budget is
	}
	return l.ordinaryLimitLocked()
}

// Outstanding is every permit held right now: O + K.
func (l *permitLedger) Outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ordinary + l.control
}

// Budget reports the current budget and the generation it belongs to.
func (l *permitLedger) Budget() (budget int, generation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budget, l.generation
}

// dialClass marks a dial as a control socket rather than ordinary work.
//
// A PRIVATE KEY TYPE, so nothing outside this package can set it. The marker
// decides which side of the budget a socket is charged to, and a value any
// caller could plant would let ordinary traffic help itself to the reserved
// lane.
type dialClassKey struct{}

// withControlDial marks a context as belonging to a control socket — today,
// a cancellation request.
func withControlDial(ctx context.Context) context.Context {
	return context.WithValue(ctx, dialClassKey{}, true)
}

func isControlDial(ctx context.Context) bool {
	v, _ := ctx.Value(dialClassKey{}).(bool)
	return v
}

// permitDialer wraps a dial function so every socket to a target takes a
// permit before it is opened and releases it when it closes.
//
// THE DIALER IS THE ONLY SEAM THAT SATISFIES the connection-budget policy, and the pool's own
// hooks do not. BeforeConnect can take a permit but has no handle on the
// connection it produced, and BeforeClose has the connection but no way back
// to the permit, so pairing them needs a correlation key the pool does not
// offer. Worse, neither fires when the DIAL ITSELF FAILS — and the ADR is
// explicit that "a dial that fails, races or is cancelled must release its
// permit", because a leaked permit is a slot lost until restart.
//
// Here the acquire, the failure path and the release are the same closure
// over the same release function, so none of them can be forgotten. It also
// gets the ADR's "every socket" rule for free: health checks, control queries
// and replacements all dial, so all of them are counted.
func permitDialer(l *permitLedger, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if l == nil {
		return dial
	}
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// A CANCELLATION IS A SECOND SOCKET, dialled through this same
		// function (pgconn.CancelRequest), and it is needed exactly when every
		// ordinary slot is spent. Charging it to the ordinary allowance would
		// remove cancellation at the only moment anyone reaches for it, so it
		// takes the reserved lane instead. The marker is set by autodb's own
		// cancellation path and cannot be forged from outside this package.
		var (
			permit *Permit
			err    error
		)
		if isControlDial(ctx) {
			permit, err = l.AcquireControl(ctx)
		} else {
			permit, err = l.Acquire()
		}
		if err != nil {
			return nil, err
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			// The dial failed, raced or was cancelled. The permit goes back
			// immediately: holding it would spend budget on a socket that
			// does not exist.
			permit.Release()
			return nil, err
		}
		return &permitConn{Conn: conn, permit: permit}, nil
	}
}

// permitConn releases its permit exactly once, whenever and however the
// socket is closed.
type permitConn struct {
	net.Conn
	permit *Permit
}

func (c *permitConn) Close() error {
	err := c.Conn.Close()
	c.permit.Release()
	return err
}
