package exec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// ErrTargetBudgetExhausted means a dial found its share of a pool's budget
// spent and waited for it as long as its caller did.
//
// It is a CAPACITY refusal and never charges the credential throttle — see
// ChargeCapacity and the charge-class policy.
var ErrTargetBudgetExhausted = errors.New("exec: target connection budget exhausted")

// ErrInvalidBudget means a live budget update was refused.
var ErrInvalidBudget = errors.New("exec: invalid target connection budget")

// DialClass says which part of a pool's budget a socket was charged to.
type DialClass uint8

const (
	// DialOrdinary is exec, TUI and internal work (queries, introspection,
	// routine refreshes, reconciliation): it draws the headroom share.
	DialOrdinary DialClass = iota
	// DialControl is the reserved lane — today, a cancellation.
	DialControl
	// DialFrontDoor is a front-door session's pin: it draws the front-door
	// share, which the session line admits sessions against (leaseScope).
	DialFrontDoor
)

func (c DialClass) String() string {
	switch c {
	case DialControl:
		return "control"
	case DialFrontDoor:
		return "front-door"
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
	// Class is which part of the budget it was charged to.
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

// permitLedger is one connection pool's socket budget: every socket autodb
// holds to the pool's targets, whatever opened it (db_pool_connections).
//
// A RUNTIME PERMIT, NOT A SUM OF CONFIGURED CAPS. A permit is taken when a
// socket is about to be dialled and released when it closes, so the budget is
// spent by connections that actually exist.
//
// TWO SHARES AND A LANE. Front-door sessions hold their backend for life, and
// exec, TUI and internal work come and go. With one shared allowance either
// side could take every socket the other needs: exec the ones the session line
// admitted front-door sessions against, or a full front door the ones a
// routine refresh needs. So each has its own share, and the line's limit IS
// the front-door share (leaseScope).
//
//	[Budget: N sockets]
//	┌──────────────────────────────────┬──────────────────────┬──────────────┐
//	│ front-door share                 │ headroom share       │ control lane │
//	│ N − 1 − poolHeadroom(N)          │ poolHeadroom(N) ≤ 4  │ 1            │
//	│ front-door session pins          │ exec, TUI, internal  │ cancels      │
//	└──────────────────────────────────┴──────────────────────┴──────────────┘
//
// A DIAL WHOSE SHARE IS FULL WAITS IN THAT SHARE'S LINE, first come first
// served, for as long as its acquirer waits (dial_marks.go). A release hands
// its slot straight to the share's oldest waiter under the ledger's lock, so
// the count never dips where another dial could step in.
type permitLedger struct {
	mu     sync.Mutex
	budget int
	// ordinary, frontDoor and control are counted SEPARATELY, so each share's
	// ceiling is stated in terms of its own occupancy and a drain converges
	// monotonically per share.
	ordinary  int
	frontDoor int
	control   int
	// generation increments on every budget change. A lowered budget cannot
	// be enforced retroactively: with 50 sockets open and a new budget of 25,
	// "outstanding <= budget" is FALSE until sockets close on their own, so
	// monotonicity is asserted per generation.
	generation uint64
	// waiting is each share's line, oldest first.
	waiting map[DialClass][]*shareWaiter

	// testBeforePublish pauses a publication while its lock is held.
	testBeforePublish func()
	// testAfterSelect pauses AFTER a generation has been chosen and BEFORE it
	// is published, inside the lock, so a cell can see a second caller block.
	testAfterSelect func()
	// controlLane serializes the reserved slot to one holder.
	controlLane chan struct{}
}

// shareWaiter is one dial waiting for its share. granted is buffered by one,
// so the releasing path never blocks on a waiter that has already given up.
type shareWaiter struct{ granted chan *Permit }

// maxPoolHeadroom caps the headroom share: exec, TUI and internal work get at
// most this many sockets of any pool, and every other socket below the control
// lane is the front door's.
const maxPoolHeadroom = 4

// poolHeadroom is THE definition of the headroom share. Nothing else computes
// it: the session line reads the front-door share through leaseScope.
func poolHeadroom(budget int) int {
	return min(max(budget, 0)/2, maxPoolHeadroom)
}

// newPermitLedger builds a ledger for exactly the budget it is given.
//
// IT NEVER RAISES A BUDGET. A number the caller did not choose is not a safe
// default just because it is small; a budget too small to be useful is REFUSED
// WHERE IT IS VALIDATED (meta.MinPoolConns, the schema's CHECK), so an operator
// gets a message naming it rather than a daemon that quietly does something
// else.
//
// THERE IS NO "UNBOUNDED" LEDGER. Absence is a nil ledger: a target outside
// every pool (MySQL, SQLite) has none, and the dialer returns the underlying
// dial untouched.
func newPermitLedger(budget int) *permitLedger {
	return &permitLedger{
		budget:      budget,
		waiting:     map[DialClass][]*shareWaiter{},
		controlLane: make(chan struct{}, 1),
	}
}

// SetBudget publishes a new budget as a new generation.
//
// IT NEVER CLOSES ANYTHING. Lowering a pool from 50 to 25 while 50 sockets are
// open must not kill twenty-five live sessions to make the number true; no new
// permit is granted above the new shares and outstanding DRAINS as work
// finishes. A raise serves both shares' lines at once.
func (l *permitLedger) SetBudget(n int) error {
	if int64(n) < meta.MinPoolConns {
		return fmt.Errorf("%w: a pool budget of %d is refused; the minimum is %d, one socket "+
			"each for the control lane, the headroom share and the front-door share",
			ErrInvalidBudget, n, meta.MinPoolConns)
	}
	// CHOSEN AND PUBLISHED IN ONE CRITICAL SECTION, so two callers can never
	// publish the same next generation or publish in the reverse order.
	l.mu.Lock()
	defer l.mu.Unlock()
	next := l.generation + 1
	if l.testAfterSelect != nil {
		l.testAfterSelect()
	}
	l.setBudgetLocked(n, next)
	return nil
}

// setBudgetLocked publishes a budget and serves both shares' lines, since a
// raise can free several slots of each. Caller holds the lock.
func (l *permitLedger) setBudgetLocked(n int, generation uint64) {
	if l.testBeforePublish != nil {
		l.testBeforePublish()
	}
	l.budget = n
	l.generation = generation
	l.serveLocked(DialFrontDoor)
	l.serveLocked(DialOrdinary)
}

// LedgerSnapshot is one coherent reading of the ledger, TAKEN UNDER ONE LOCK,
// because the fields only mean something together.
type LedgerSnapshot struct {
	// Configured is the operator's number: the pool's max_conns.
	Configured int
	// FrontDoorLimit and OrdinaryLimit are the two shares; the control lane
	// is the one socket left.
	FrontDoorLimit int
	OrdinaryLimit  int
	// FrontDoor, Ordinary and Control are the three occupancies.
	FrontDoor int
	Ordinary  int
	Control   int
	// FrontDoorWaiting and OrdinaryWaiting are each share's line.
	FrontDoorWaiting int
	OrdinaryWaiting  int
	// Outstanding is every socket the pool holds. It may EXCEED Configured
	// while a lowered budget drains; that is not an error.
	Outstanding int
	// Effective is the immediately exercisable ceiling: max(Configured,
	// FrontDoor+Ordinary+1). The +1 is the control lane, counted whether or
	// not a cancel is in flight, so the number can't oscillate in a drain.
	Effective int
	// Generation increments on every accepted update.
	Generation uint64
	// Draining reports that a share holds more than its current limit.
	Draining bool
}

// Snapshot returns a coherent reading of the ledger.
func (l *permitLedger) Snapshot() LedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LedgerSnapshot{
		Configured:       l.budget,
		FrontDoorLimit:   l.frontDoorShareLocked(),
		OrdinaryLimit:    l.ordinaryLimitLocked(),
		FrontDoor:        l.frontDoor,
		Ordinary:         l.ordinary,
		Control:          l.control,
		FrontDoorWaiting: len(l.waiting[DialFrontDoor]),
		OrdinaryWaiting:  len(l.waiting[DialOrdinary]),
		Outstanding:      l.frontDoor + l.ordinary + l.control,
		Effective:        l.effectiveLocked(),
		Generation:       l.generation,
		Draining:         l.frontDoor > l.frontDoorShareLocked() || l.ordinary > l.ordinaryLimitLocked(),
	}
}

// FrontDoorShare is how many front-door sessions the pool's line may admit:
// each holds one backend for life, drawn from this share.
func (l *permitLedger) FrontDoorShare() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frontDoorShareLocked()
}

// acquire takes a permit of the mark's class, or joins that share's line and
// waits as long as the acquirer does. Not for the control lane: AcquireControl.
func (l *permitLedger) acquire(m dialMark) (*Permit, error) {
	class := m.class()
	ctx, cancel := m.waitContext()
	defer cancel()

	l.mu.Lock()
	// NOBODY STEPS AROUND THE LINE. A free slot goes to an arrival only when
	// nobody of its class is already waiting for one.
	if len(l.waiting[class]) == 0 {
		if p, ok := l.grantLocked(class); ok {
			l.mu.Unlock()
			return p, nil
		}
	}
	w := &shareWaiter{granted: make(chan *Permit, 1)}
	l.waiting[class] = append(l.waiting[class], w)
	l.mu.Unlock()

	select {
	case p := <-w.granted:
		return p, nil
	case <-ctx.Done():
		if !l.leaveLine(class, w) {
			// Granted in the same instant: the slot is ours, and nobody wants
			// it any more, so it goes straight back (and on to the next waiter).
			(<-w.granted).Release()
		}
		return nil, ErrTargetBudgetExhausted
	}
}

// leaveLine removes w from its share's line, reporting whether it was still
// there (false: it was granted first).
func (l *permitLedger) leaveLine(class DialClass, w *shareWaiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := l.waiting[class]
	for i, x := range line {
		if x == w {
			l.waiting[class] = append(line[:i:i], line[i+1:]...)
			return true
		}
	}
	return false
}

// AcquireControl takes the RESERVED slot: the one no share can have.
//
// A cancellation is a second, short-lived socket to the same server, needed
// exactly when every other slot is spent. It WAITS for the lane rather than
// refusing it, bounded by the caller's own context, because an immediate
// refusal would silently lose a second concurrent cancel.
func (l *permitLedger) AcquireControl(ctx context.Context) (*Permit, error) {
	select {
	case l.controlLane <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l.mu.Lock()
	p, _ := l.grantLocked(DialControl) // the lane is its bound: grantLocked never refuses it
	l.mu.Unlock()
	inner := p.release
	p.release = func() {
		inner()
		<-l.controlLane
	}
	return p, nil
}

// grantLocked counts one permit of class if its share has room, and builds it.
// THE ONE GRANT PATH: an arrival, a hand-off to a waiter and the control lane
// all come through here. Caller holds the lock.
//
// THE CONTROL LANE DOES NOT TEST A LIMIT HERE. Its bound is the lane semaphore,
// and testing outstanding would refuse a cancel in the one state it matters
// most: while a lowered budget drains.
func (l *permitLedger) grantLocked(class DialClass) (*Permit, bool) {
	held := l.heldLocked(class)
	if class != DialControl && *held >= l.limitForLocked(class) {
		return nil, false
	}
	*held++
	p := &Permit{
		Generation:       l.generation,
		Class:            class,
		ConfiguredTotal:  l.budget,
		EffectiveCeiling: l.effectiveLocked(),
		Limit:            l.limitForLocked(class),
	}
	p.release = func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if *held > 0 {
			*held--
		}
		// THE FREED SLOT GOES TO WHOEVER ASKED FIRST, HERE, NOT ON A TIMER,
		// and only within the class that freed it: a share's slot is never
		// handed to another share, and the control lane feeds no line.
		if class != DialControl {
			l.serveLocked(class)
		}
	}
	return p, true
}

// serveLocked hands free slots of a share to its oldest waiters, looping while
// the share has room and its line isn't empty. A share's release calls it for
// its share; a budget publication calls it for both. Caller holds the lock.
func (l *permitLedger) serveLocked(class DialClass) {
	for len(l.waiting[class]) > 0 {
		p, ok := l.grantLocked(class)
		if !ok {
			return
		}
		w := l.waiting[class][0]
		l.waiting[class] = l.waiting[class][1:]
		w.granted <- p
	}
}

func (l *permitLedger) heldLocked(class DialClass) *int {
	switch class {
	case DialControl:
		return &l.control
	case DialFrontDoor:
		return &l.frontDoor
	}
	return &l.ordinary
}

// frontDoorShareLocked is the budget less the control lane and the headroom:
// 25 → 20, 3 → 1. Caller holds the lock.
func (l *permitLedger) frontDoorShareLocked() int {
	return max(l.budget-1-poolHeadroom(l.budget), 0)
}

// ordinaryLimitLocked is the headroom share. Caller holds the lock.
func (l *permitLedger) ordinaryLimitLocked() int {
	return poolHeadroom(l.budget)
}

// limitForLocked is the allowance in force for a class at grant time.
func (l *permitLedger) limitForLocked(class DialClass) int {
	switch class {
	case DialControl:
		return 1 // the lane is one holder, whatever the budget is
	case DialFrontDoor:
		return l.frontDoorShareLocked()
	}
	return l.ordinaryLimitLocked()
}

// effectiveLocked is the immediately exercisable ceiling: max(Configured,
// shares held + 1). Caller holds the lock.
func (l *permitLedger) effectiveLocked() int {
	return max(l.frontDoor+l.ordinary+1, l.budget)
}

// Outstanding is every permit held right now.
func (l *permitLedger) Outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frontDoor + l.ordinary + l.control
}

// Budget reports the current budget and the generation it belongs to.
func (l *permitLedger) Budget() (budget int, generation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budget, l.generation
}

// permitDialer wraps a dial function so every socket to a pooled target takes a
// permit before it is opened and releases it when it closes.
//
// THE DIALER IS THE ONLY SEAM THAT SATISFIES the connection-budget policy, and
// the pool's own hooks do not: neither BeforeConnect nor BeforeClose fires when
// the DIAL ITSELF FAILS, and a permit leaked there is a slot lost until restart.
// Here the acquire, the failure path and the release are one closure, and every
// socket dials, health checks and replacements included, so all are counted.
func permitDialer(l *permitLedger, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if l == nil {
		return dial
	}
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		m := dialMarkOf(ctx)
		var (
			permit *Permit
			err    error
		)
		// A CANCELLATION IS A SECOND SOCKET, needed exactly when every share
		// is spent, so it takes the reserved lane under its own deadline.
		if m.class() == DialControl {
			permit, err = l.AcquireControl(m.acquirer)
		} else {
			permit, err = l.acquire(m)
		}
		if err != nil {
			return nil, err
		}
		m.noteGranted()
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
