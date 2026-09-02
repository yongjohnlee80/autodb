package frontdoor

import (
	"fmt"
	"sync"
	"time"
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
// backend, which is the failure mode PR #52 r1 MF7 was about in a different
// clothing; the bound is a policy choice recorded in
// docs/front-door/session-loop-budgets.md rather than a matrix figure.
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
const DefaultGeneralLaneBytes int64 = 1 << 30

// generalLaneWaitBudget bounds how long one connection waits for another to
// release before it stops holding a statement open. Policy, not a matrix figure.
const generalLaneWaitBudget = 30 * time.Second

// GENERAL-LANE FLOOR (§1.4's composition rule, jarvis's ruling 2c, 2026-09-03).
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
// So the floor is DERIVED rather than asserted, and startup fails below it. If a
// future change raises the watermark or the session cap, the build stops instead
// of silently over-committing a lane that three separate documents describe as
// having room.
//
// WHAT THIS FLOOR DOES NOT YET COVER, stated here rather than left implicit:
// §1.4 charges THREE things to this lane — pending output, segment input, and
// retained statement/portal state. Only pending output is a RESERVATION; the
// other two are per-session CAPS (retained state 16 MiB, segment 96 MiB, §9),
// and a cap is not a reservation — nothing takes them up front. Composing the
// caps the way this composes the reservation gives 256 × 116 MiB ≈ 29 GiB
// against a published ceiling of 4 GiB, which is itself the evidence that they
// were never meant to compose that way. Completing the rule needs a per-session
// reservation figure for those two, or a ruling that they stay caps. Raised with
// the lead; the floor below is the part that can be derived from what is
// published today.
const (
	// generalLaneSessionCap is the global session cap the floor composes over
	// (config.DefaultMaxSessionsGlobal; matrix row 2.7).
	generalLaneSessionCap = 256

	// generalLaneCeiling is §9's ceiling for the global budget.
	generalLaneCeiling int64 = 4 << 30
)

// GeneralLaneFloor is the smallest lane that lets every session hold one output
// working set at full occupancy.
func GeneralLaneFloor() int64 {
	return int64(generalLaneSessionCap) * pendingOutputWatermark
}

// validateGeneralLane enforces the composition rule: config may only RAISE the
// lane, never lower it below the floor.
func validateGeneralLane(bytes int64) error {
	floor := GeneralLaneFloor()
	if bytes < floor {
		return fmt.Errorf("general lane %d bytes is below the floor of %d (%d sessions × %d watermark): "+
			"at full occupancy a session could not hold one output working set, and the lane would refuse "+
			"statements that nothing is wrong with",
			bytes, floor, generalLaneSessionCap, pendingOutputWatermark)
	}
	if bytes > generalLaneCeiling {
		return fmt.Errorf("general lane %d bytes exceeds §9's ceiling of %d", bytes, generalLaneCeiling)
	}
	return nil
}
