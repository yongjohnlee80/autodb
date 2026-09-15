package exec

import (
	"context"
	"errors"
	"net"
	"sync"
)

// ErrTargetBudgetExhausted means this instance already holds every production
// connection its operator allowed it to hold.
//
// It is a CAPACITY refusal and never charges the credential throttle — see
// ChargeCapacity and the charge-class policy.
var ErrTargetBudgetExhausted = errors.New("exec: target connection budget exhausted")

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
	mu          sync.Mutex
	budget      int
	outstanding int
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
func (l *permitLedger) SetBudget(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budget = n
	l.generation++
}

// Acquire takes one permit, or reports that the budget is spent.
//
// The returned release is IDEMPOTENT. A permit released twice would hand out a
// slot that does not exist; one never released is a slot lost for the life of
// the process. Both are silent, so the safe shape is a release that can be
// called from every unwind path — dial failure, cancellation, close — without
// the caller tracking whether it already ran.
func (l *permitLedger) Acquire() (release func(), err error) {
	return l.acquire(false)
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
func (l *permitLedger) AcquireControl() (release func(), err error) {
	select {
	case l.controlLane <- struct{}{}:
	default:
		return nil, ErrTargetBudgetExhausted
	}
	rel, err := l.acquire(true)
	if err != nil {
		<-l.controlLane
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			rel()
			<-l.controlLane
		})
	}, nil
}

func (l *permitLedger) acquire(control bool) (release func(), err error) {
	l.mu.Lock()
	// ORDINARY DIALS STOP ONE SHORT. The last permit is the control lane's,
	// so a cancellation can always be delivered.
	limit := l.budget
	if !control && limit > 1 {
		limit--
	}
	if l.budget > 0 && l.outstanding >= limit {
		l.mu.Unlock()
		return nil, ErrTargetBudgetExhausted
	}
	l.outstanding++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.outstanding > 0 {
				l.outstanding--
			}
			l.mu.Unlock()
		})
	}, nil
}

// Outstanding is how many permits are held right now.
func (l *permitLedger) Outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.outstanding
}

// Budget reports the current budget and the generation it belongs to.
func (l *permitLedger) Budget() (budget int, generation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budget, l.generation
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
		release, err := l.Acquire()
		if err != nil {
			return nil, err
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			// The dial failed, raced or was cancelled. The permit goes back
			// immediately: holding it would spend budget on a socket that
			// does not exist.
			release()
			return nil, err
		}
		return &permitConn{Conn: conn, release: release}, nil
	}
}

// permitConn releases its permit exactly once, whenever and however the
// socket is closed.
type permitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *permitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
