package frontdoor

import (
	"fmt"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// THE GENERAL LANE — the process-wide resident-memory budget (matrix §1.4, §8.1).
//
// The control lane already existed: 64 KiB per connection, reserved atomically
// at accept, released at close, so a saturated process can always still process
// the messages that RELEASE capacity. This is its counterpart — the general
// lane, default 1 GiB, against which segment input, retained statement/portal
// state and pending serialized output are charged.
//
// F1 charges one of those three: pending serialized output. The per-connection
// 4 MiB watermark paces ONE connection; it says nothing about a thousand
// connections each holding four megabytes, which is four gigabytes of resident
// output in a process budgeted for one. A per-connection bound cannot express a
// process-wide limit, which is why the matrix asks for both.
//
// SATURATION IS BACKPRESSURE, NEVER AN ERROR (§7): reads pause, audited. That is
// the whole design — a connection that cannot reserve waits for one that can
// release, and the thing it does while waiting is flush, which is itself a
// release. A budget whose remedy is refusal would turn a busy moment into a
// failed statement.

// generalLane is the process-wide general budget. Reserve before serializing,
// release when the bytes reach the socket.
type generalLane struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int64
	used  int64
}

// newGeneralLane constructs a process-wide general memory lane with the given byte ceiling.
func newGeneralLane(limit int64) *generalLane {
	l := &generalLane{limit: limit}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// tryReserve takes n if the lane has room, reporting whether it did. It never
// blocks, so a caller can decide whether to flush first and try again.
func (l *generalLane) tryReserve(n int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used+n > l.limit {
		return false
	}
	l.used += n
	return true
}

// reserve takes n, waiting up to budget for another connection to release.
//
// It reports whether the reservation succeeded. A false is NOT an error the peer
// is told about — §7 says the general budget produces backpressure, never an
// error — it means the wait was longer than a caller is willing to hold a
// statement open for, which is a decision the caller makes with its own context.
//
// The wait is bounded rather than indefinite. An unbounded wait on a lane that
// nothing releases is a hung session holding the engine's claim and a pinned
// backend, which is the failure mode an earlier review was about in a different
// clothing; the bound is a policy choice recorded in
// the session-loop budgets reference in the KB (shared/reference/
// autodb-front-door-session-loop-budgets.md) rather than a matrix figure.
func (l *generalLane) reserve(n int64, budget time.Duration, now func() time.Time) bool {
	if n > l.limit {
		// Larger than the lane itself: no amount of waiting can admit it, and
		// waiting would be a deadlock dressed as patience.
		return false
	}
	deadline := now().Add(budget)

	// A timer wakes the wait, because sync.Cond has no deadline of its own and a
	// releaser that never comes would otherwise park the caller for good.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTimer(budget)
		defer t.Stop()
		select {
		case <-t.C:
			l.cond.Broadcast()
		case <-stop:
		}
	}()

	l.mu.Lock()
	defer l.mu.Unlock()
	for l.used+n > l.limit {
		if !now().Before(deadline) {
			return false
		}
		l.cond.Wait()
	}
	l.used += n
	return true
}

// release returns n to the lane and wakes whoever is waiting for it.
func (l *generalLane) release(n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	l.used -= n
	if l.used < 0 {
		// A release without a matching reservation is a bookkeeping bug, and
		// clamping hides it from every later reading. Clamp — the alternative is
		// a negative budget that admits everything — but do not pretend it is
		// normal: the invariant is that release is paired with reserve.
		l.used = 0
	}
	l.mu.Unlock()
	l.cond.Broadcast()
}

// capacity reports the lane's limit, so a caller can clamp a reservation to
// something the lane could ever admit rather than asking for a wait that can
// only time out.
func (l *generalLane) capacity() int64 { return l.limit }

// inUse reports the bytes currently reserved, for cells and diagnostics.
func (l *generalLane) inUse() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

// DefaultGeneralLaneBytes is the matrix's 1 GiB general budget (§1.4).
//
// Defined in core/config, which is the layer an operator sets and this package
// already reads, and referred to here so the figure is one literal rather than
// two that agree until someone edits one.
const DefaultGeneralLaneBytes = config.DefaultGeneralLaneBytes

// generalLaneWaitBudget bounds how long one connection waits for another to
// release before it stops holding a statement open. Policy, not a matrix figure.
const generalLaneWaitBudget = 30 * time.Second

// GENERAL-LANE FLOOR (matrix §1.4's composition rule, ruled 2026-09-03).
//
// §1.4 already binds the CONTROL lane this way — its default is
// `max_frontend_connections × 64 KiB`, and config may only raise it — so that
// the reservation every connection is entitled to cannot be an accident of three
// constants happening to multiply out. The general lane needs the same treatment
// for the same reason, and the arithmetic is why: the pre-dispatch reservation is
// one watermark per in-flight statement, so at the global session cap the lane is
// committed to 256 × 4 MiB = 1 GiB — EXACTLY today's default, with zero margin.
// It fits by coincidence, not by construction.
//
// So the floor is DERIVED rather than asserted, and startup fails below it.
//
// IT NOW DERIVES FROM THE CONFIGURED CAP, WHICH IS WHAT MAKES THAT TRUE.
// It used to multiply a local literal 256 that no configuration reached, so the
// claim above was prose rather than mechanism: an operator who lowered
// exec.max_sessions_global to fit a smaller host got no reduction in the floor,
// and the lane went on demanding a gibibyte the machine did not have. Worse, the
// literal duplicated config.DefaultMaxSessionsGlobal without referring to it, so
// changing the real cap would have left the floor describing an occupancy that no
// longer existed — silently, because nothing related the two values. A comment
// asserting a relation is not a mechanism; the parameter is.
//
// The composition rule is unchanged and still one-directional: config may only
// RAISE the lane above this floor. Lowering the SESSION CAP is how a modest host
// legitimately asks for a smaller lane — it reduces occupancy and the floor with
// it, rather than promising an occupancy the budget cannot serve.
//
// WHY ONLY ONE OF THE THREE CHARGES COMPOSES (matrix §1.4, RULED 2026-09-03).
// §1.4 charges three things to this lane, and only pending output is a
// RESERVATION — it alone has a knowable bound per statement, the watermark.
// Segment input (96 MiB per segment) and retained state (16 MiB per session) are
// CAPS on one session, charged as they occur and admitted by backpressure; their
// sum over the session cap is ≈29 GiB against a 4 GiB ceiling, which is the proof
// they were never a composition. A cap bounds what ONE session may hold; the lane
// bounds what ALL sessions hold together, and §7's backpressure is how the second
// is enforced when the sum of the caps exceeds it — which it always did, by
// design.

// generalLaneCeiling is matrix §9's ceiling for the global budget. One literal,
// in core/config, for the same reason as the default above.
const generalLaneCeiling = config.MaxGeneralLaneBytes

// GeneralLaneFloor is the smallest lane that lets every session hold one output
// working set at full occupancy.
//
// sessionCap is the EFFECTIVE exec.max_sessions_global. A non-positive value
// takes config.DefaultMaxSessionsGlobal, matching every other unset-takes-the-
// default limit on this surface, so a caller that has not resolved its config
// yet gets the shipped occupancy rather than a floor of zero — which would admit
// any lane at all and quietly delete the guard.
func GeneralLaneFloor(sessionCap int) int64 {
	if sessionCap <= 0 {
		sessionCap = config.DefaultMaxSessionsGlobal
	}
	return int64(sessionCap) * pendingOutputWatermark
}

// validateGeneralLane enforces the composition rule: config may only RAISE the
// lane, never lower it below the floor full occupancy needs.
func validateGeneralLane(bytes int64, sessionCap int) error {
	floor := GeneralLaneFloor(sessionCap)
	if bytes < floor {
		return fmt.Errorf("general lane %d bytes is below the floor of %d (%d sessions × %d watermark): "+
			"at full occupancy a session could not hold one output working set, and the lane would refuse "+
			"statements that nothing is wrong with; lower exec.max_sessions_global to lower the floor",
			bytes, floor, effectiveSessionCap(sessionCap), pendingOutputWatermark)
	}
	if bytes > generalLaneCeiling {
		return fmt.Errorf("general lane %d bytes exceeds matrix §9's ceiling of %d", bytes, generalLaneCeiling)
	}
	return nil
}

// effectiveSessionCap resolves the cap the way GeneralLaneFloor does, so the
// error message names the occupancy the floor was actually computed from rather
// than the caller's zero.
func effectiveSessionCap(sessionCap int) int {
	if sessionCap <= 0 {
		return config.DefaultMaxSessionsGlobal
	}
	return sessionCap
}
