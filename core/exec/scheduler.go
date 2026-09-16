package exec

import (
	"context"
	"errors"
	"time"
)

// A FRONT-DOOR REQUEST THAT CANNOT HAVE A CONNECTION NOW WAITS ITS TURN,
// INSTEAD OF BEING TOLD THE SYSTEM IS FULL.
//
// Until this, a developer arriving at a saturated target was refused outright:
// admitWithLease found the lease cap reached and the front door answered "too
// many connections". That answer is correct and useless. The capacity it
// reports is almost always transient — a colleague's session is a second from
// closing — so the refusal turns a one-second wait into a failed connection,
// and the developer retries in a loop that makes the contention worse.
//
// It is also unfair in a way no amount of capacity fixes. Whoever happens to
// retry at the microsecond a lease frees gets it, so a person who asked first
// can lose repeatedly to a client that reconnects in a tight loop. The
// unfairness lives in the scheduling, not in the supply.
//
// THE LINE IS INSTANCE-WIDE AND THE REGISTRY'S OWN MUTEX OWNS IT. That mutex
// already owns every cap this schedules against — session slots, per-user
// slots, wire leases, the resident budget — and admitWithLease already takes
// all four as one operation because a partial reservation is impossible to
// unwind safely. Putting the line under the same lock means a freed lease
// moves to the next waiter WITHOUT ever being observable as free: there is no
// window between "released" and "granted" for a newcomer to win the race a
// queue exists to abolish.

// waitState is what happened to one queued request. It changes only under the
// registry mutex, which is what makes a grant and a cancellation arriving in
// the same instant resolve to exactly one of them.
type waitState int

const (
	waitQueued waitState = iota
	// waitResolved means the line has already decided this request's outcome
	// and delivered it. A caller that gives up after this point does not get to
	// pretend it was never served: if it was admitted, its admission must be
	// given back, or the session it holds is one nobody will ever close.
	waitResolved
)

// admitWaiter is one front-door request's place in line, holding everything
// needed to admit it WITHOUT waking it first.
//
// THE RELEASING PATH PERFORMS THE ADMISSION ON THE WAITER'S BEHALF. Waking a
// waiter to let it retry is what creates the gap: between the wake and the
// retry, any newcomer can take the slot, and the oldest request can lose its
// turn arbitrarily many times. Carrying the session and its charges here means
// the handoff is a single operation under the mutex.
type admitWaiter struct {
	s         *session
	leaseConn int64
	overhead  int64
	seq       uint64
	state     waitState
	// blockedBy is the most recent capacity refusal that passed this waiter
	// over. Guarded by r.mu, like everything else about the line.
	blockedBy error
	// done carries the outcome. Buffered by one so the releasing path never
	// blocks on a caller that has already given up.
	done chan error
}

// resolve delivers one outcome. Caller holds r.mu.
func (w *admitWaiter) resolve(err error) {
	w.state = waitResolved
	w.done <- err
}

// admitWithLeaseOrWait admits a front-door session, or waits its turn.
//
// IT IS THE ONLY DOOR FOR WIRE SESSIONS. admitWithLease remains for the
// internal surfaces, which have no client to keep waiting and want the
// immediate answer.
func (r *sessionRegistry) admitWithLeaseOrWait(ctx context.Context, s *session, leaseConn, overhead int64) error {
	w := &admitWaiter{s: s, leaseConn: leaseConn, overhead: overhead, done: make(chan error, 1)}

	r.mu.Lock()
	if r.closed != nil {
		r.mu.Unlock()
		return r.closed
	}

	// JOIN THE LINE FIRST, THEN DISPATCH, BOTH UNDER THIS ONE LOCK.
	//
	// Joining unconditionally is what stops a newcomer stepping around
	// requests that are already waiting: there is no path that takes capacity
	// without going through the order. Dispatching in the same critical
	// section is what stops the opposite failure -- a request whose own target
	// is free sitting behind waiters for a target that is full, waiting for an
	// unrelated release that might never come. serveLine admits the oldest
	// ELIGIBLE request, so this request is admitted immediately if nothing
	// older can use the capacity it is asking for, and waits otherwise.
	r.lineSeq++
	w.seq = r.lineSeq
	r.line = append(r.line, w)
	r.serveLine()
	if w.state == waitResolved {
		r.mu.Unlock()
		return <-w.done
	}

	// NOTHING IS COMING, SO NOBODY IS MADE TO WAIT. Every lease on the target
	// is held by a transaction still inside its bounds: no release is pending,
	// so the wait would end ninety seconds later with the answer available
	// now. Decided HERE and only here -- a request that goes on to wait never
	// receives this identity, because a record claiming a request never waited
	// has to be true of that request.
	if r.allLeasesInTransactionLocked(leaseConn) {
		r.dropFromLineLocked(w)
		r.mu.Unlock()
		return ErrAllCapacityInTransaction
	}

	hook := r.hookWaiterQueued
	demand := r.onDemand
	r.mu.Unlock()

	// ASK ONE IDLE HOLDER TO LEAVE, ONCE.
	//
	// Reached only by a request that is genuinely waiting and that row 5 did
	// not refuse, so the target is full of holders that are not going to
	// release on their own. Attempted ONCE rather than in a loop: a loop would
	// turn one request's arrival into a sweep that ends every idle session on
	// the target, when one lease is all that was asked for. Sustained pressure
	// is the line's job, and the line is already holding this request.
	//
	// Outside the lock because selecting a victim reads sessions, and the freed
	// lease is NOT handed back here -- it goes through the line like any other
	// release, so demand cannot become a way to jump the queue.
	if demand != nil {
		demand(leaseConn)
	}

	if hook != nil {
		// Announced OUTSIDE the lock and only once the request is genuinely
		// waiting, so a cell can act on that fact instead of polling for it.
		hook(w.seq)
	}

	// WHICH BOUND OWNS THIS WAIT IS DECIDED HERE, ONCE, FROM THE TWO ABSOLUTE
	// DEADLINES -- not by racing two channels and reporting whichever wins.
	//
	// Both bounds apply: a client with five seconds left does not get ninety.
	// But a select among cases that are ALREADY READY picks at random, so when
	// the two deadlines coincide -- exactly what happens when the caller's is
	// the earlier one and the server timer is armed to it -- the answer was a
	// coin toss, and half the time a client that had run out of its own time
	// was told the SERVER's wait had expired.
	//
	// So exactly one bound is armed. If the caller's deadline arrives no later
	// than the server's, the caller's context owns the wait ALONE and the
	// server's expiry channel stays nil, which blocks forever in a select. At
	// exact equality the caller owns it, because "you ran out of time" is the
	// more specific and more useful answer. Cancellation stays live either way.
	serverDeadline := r.clock().Add(queueWait)
	var serverExpiry <-chan time.Time
	if d, ok := ctx.Deadline(); !ok || d.After(serverDeadline) {
		timer := r.serverTimer(queueWait)
		defer timer.Stop()
		serverExpiry = timer.C
	}

	select {
	case err := <-w.done:
		return err
	case <-ctx.Done():
		return r.giveUp(w, context.Cause(ctx))
	case <-serverExpiry:
		return r.giveUp(w, r.expiredWaitReason(w))
	}
}

// giveUp ends a wait on the caller's terms, and hands back an admission that
// was granted in the same instant.
//
// THE GRANT WINS THE RACE AND IS THEN UNDONE, rather than the cancellation
// simply being reported. A request removed from the line by the releasing path
// has ALREADY been admitted: its session is in the registry and its lease,
// session slot and memory charge are all spent. Returning the caller's
// cancellation without giving those back would leak a session that nothing is
// coming to close — the target would refuse connections it has capacity for,
// which is the original incident wearing a different hat.
func (r *sessionRegistry) giveUp(w *admitWaiter, own error) error {
	if h := r.hookGivingUp; h != nil {
		// INSIDE THE WINDOW. The caller has stopped waiting but has not yet
		// left the line, which is the only instant in which a grant can still
		// reach it -- and therefore the only instant in which the undo below
		// matters. A cell that cannot stand here has to hope the race lands,
		// and a race that has to be hoped for is not a test.
		h()
	}
	if r.leaveLine(w) {
		return own // still queued, so nothing was ever taken
	}
	// The line resolved it first. Take that outcome and undo it if it was an
	// admission; a refusal needs nothing given back.
	if err := <-w.done; err == nil {
		r.remove(w.s)
	}
	return own
}

// QueueTimeoutError is a wait that reached the server's bound without being
// served, carrying the cap that held it as DIAGNOSIS rather than as identity.
//
// IT UNWRAPS TO ErrQueueTimeout AND TO NOTHING ELSE, and that restraint is the
// whole design. The obvious shape -- wrap both the timeout and the blocking cap
// so errors.Is answers yes to each -- is what this replaced, and it was wrong
// in a way that looked like extra information: the front door tests the cap
// arms before the timeout arm, so EVERY expiry rendered as a plain cap refusal
// and the queue-timeout identity became unreachable in production while
// remaining registered. That is a retroactive relabel. A request that waited
// ninety seconds would have been recorded as though it had been refused on
// arrival, which is exactly the record the ruling forbids: once a request is in
// the line it resolves by being admitted, by the caller giving up, or by its
// wait expiring, and never by acquiring a refusal that claims it never waited.
//
// The cap is still worth knowing -- an operator's remedy for a full target pool
// differs from one for a full session cap -- so it is reachable through
// BlockedBy, which the audit projection asks for deliberately and no ordered
// errors.Is switch can select by accident.
type QueueTimeoutError struct{ blockedBy error }

func (e *QueueTimeoutError) Error() string {
	if e.blockedBy == nil {
		return ErrQueueTimeout.Error()
	}
	return ErrQueueTimeout.Error() + " (blocked by: " + e.blockedBy.Error() + ")"
}

// Unwrap yields the timeout ALONE. See the type's comment.
func (e *QueueTimeoutError) Unwrap() error { return ErrQueueTimeout }

// BlockedBy is the cap that held this request, for the operator-facing record.
// Nil when the wait expired without any cap having passed the request over.
func (e *QueueTimeoutError) BlockedBy() error { return e.blockedBy }

// expiredWaitReason builds the answer for a wait that was never reached.
func (r *sessionRegistry) expiredWaitReason(w *admitWaiter) error {
	r.mu.Lock()
	blocked := w.blockedBy
	r.mu.Unlock()
	return &QueueTimeoutError{blockedBy: blocked}
}

// leaveLine takes a waiter out of the line, reporting whether it was still
// there. Its answer is the ownership decision: exactly one of the line and the
// caller can find it queued.
func (r *sessionRegistry) leaveLine(w *admitWaiter) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropFromLineLocked(w)
}

// dropFromLineLocked takes a waiter out of the line, reporting whether it was
// still there. Caller holds r.mu.
func (r *sessionRegistry) dropFromLineLocked(w *admitWaiter) bool {
	for i, x := range r.line {
		if x == w {
			r.line = append(r.line[:i], r.line[i+1:]...)
			w.state = waitResolved
			return true
		}
	}
	return false
}

// serveLine admits everyone in the line who can be admitted now. Caller holds
// r.mu, and that is the whole point: it runs in the same critical section as
// the release that freed the capacity.
//
// OLDEST *ELIGIBLE* FIRST, WHICH IS NOT THE SAME AS OLDEST. A waiter for a
// target that is still full cannot be admitted, and stopping at it would let
// one saturated target block every waiter for every other target — head-of-line
// blocking that would make the queue worse than the refusal it replaced. So it
// is skipped and KEEPS ITS PLACE: it is not moved to the back, and the next
// release still finds it ahead of everyone who arrived later.
func (r *sessionRegistry) serveLine() {
	for served := true; served; {
		served = false
		for i, w := range r.line {
			err := r.admitLocked(w.s, w.leaseConn, w.overhead)
			if isTransientCapacity(err) {
				// Remembered so an expired wait can name the cap that held it
				// rather than only the fact that it waited.
				w.blockedBy = err
				continue // not now; the place in line is kept
			}
			r.line = append(r.line[:i], r.line[i+1:]...)
			// A durable refusal is delivered to the waiter rather than
			// swallowed: the client is entitled to the real reason, and
			// leaving it in line would hold a connection open forever for a
			// cap that will never clear.
			w.resolve(err)
			served = true
			break
		}
	}
}

// isTransientCapacity reports whether waiting is the right answer to this
// refusal.
//
// ONLY THE TARGET'S LEASE CAP, and deliberately not the instance-wide caps
// beside it. The lease cap is the one a colleague's session closing clears in
// seconds, and it is the one the incident was about. A global session cap, a
// per-user cap or an exhausted memory budget are facts about the instance or
// about the caller's own footprint: holding a client open for ninety seconds
// to tell it the same thing is worse than telling it now.
func isTransientCapacity(err error) bool {
	return errors.Is(err, ErrLeaseCapExceeded)
}

// closeLine refuses every waiter and every later arrival, once.
//
// WITHOUT IT, SHUTDOWN IS SILENTLY SLOW AND THEN WRONG. A waiter with no
// deadline of its own would sit for the full ninety seconds after the instance
// stopped serving, and a release landing in that window would admit it onto
// pools that are already closing.
func (r *sessionRegistry) closeLine() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed == nil {
		r.closed = ErrEngineClosing
	}
	n := len(r.line)
	for _, w := range r.line {
		w.resolve(r.closed)
	}
	r.line = nil
	return n
}

// beginDrainingTarget marks a connection as shutting down, answers everyone
// waiting for it, and returns the sessions that are on it — as ONE transition.
//
// ONE CRITICAL SECTION, AND THAT IS THE WHOLE POINT. Marking and sweeping used
// to be two calls, each taking this lock on its own, and the gap between them
// was a hole a request could fall into: it could join the line AFTER the sweep
// had answered every waiter and BEFORE draining was true, which left it waiting
// on a connection that no longer exists with nothing left to tell it so. It
// would have sat there until its wait expired and then been told the instance
// was busy, which is false and sends whoever reads the trail looking for
// capacity pressure that never happened.
//
// Done together, the transition has no inside. Every request is on exactly one
// side of it: already waiting, and answered here; or arriving afterwards, and
// refused by the draining mark before it can join anything.
func (r *sessionRegistry) beginDrainingTarget(connID int64) (int, []*session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// MARK FIRST. Everything after this point in this critical section is
	// cleanup of what was already there; nothing new can arrive behind it.
	r.draining[connID] = true

	kept := r.line[:0]
	dropped := 0
	for _, w := range r.line {
		if w.leaseConn == connID {
			// ANSWERED AT THE MOMENT OF REMOVAL, not by letting the wait
			// expire. A timeout invites a retry that can now never succeed;
			// this says the thing they asked for no longer exists. The send
			// cannot block: the outcome channel is buffered by one and each
			// waiter is resolved exactly once.
			w.resolve(ErrTargetGone)
			dropped++
			continue
		}
		// Waiters for other connections are untouched: removing one target
		// says nothing about the rest.
		kept = append(kept, w)
	}
	r.line = kept

	var on []*session
	for _, s := range r.byID {
		if s.connID == connID {
			on = append(on, s)
		}
	}
	return dropped, on
}

// lineDepth reports how many requests are waiting, for the pressure view.
func (r *sessionRegistry) lineDepth() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.line)
}

// lineDepthFor reports how many of them are waiting on one connection.
func (r *sessionRegistry) lineDepthFor(connID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.line {
		if w.leaseConn == connID {
			n++
		}
	}
	return n
}

// serverTimer builds the timer for the server's wait.
//
// A SEAM RATHER THAN A FAKE CLOCK. The clock decides which bound OWNS the wait;
// the timer decides when it actually fires, and no amount of moving wall time
// makes a real time.Timer fire sooner. A cell that wants to watch a wait expire
// has to be able to expire it, and the alternative -- letting the cell sleep
// through ninety real seconds -- is how a focused gate becomes something people
// stop running.
func (r *sessionRegistry) serverTimer(d time.Duration) *time.Timer {
	if r.newTimer != nil {
		return r.newTimer(d)
	}
	return time.NewTimer(d)
}

// clock is the registry's time source, injectable so a cell can drive the wait
// without sleeping through it.
func (r *sessionRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
