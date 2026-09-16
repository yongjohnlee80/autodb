package exec

import (
	"context"
	"errors"
	"fmt"
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
	r.mu.Unlock()
	if hook != nil {
		// Announced OUTSIDE the lock and only once the request is genuinely
		// waiting, so a cell can act on that fact instead of polling for it.
		hook(w.seq)
	}

	// THE TIMER CARRIES THE SERVER'S WAIT ONLY, and the caller's deadline is
	// left to the caller's own context.
	//
	// IT USED TO CARRY THE EARLIER OF THE TWO, which was wrong in a way that
	// shows up as a wrong answer rather than as a hang: when the caller's
	// deadline was the earlier one, the timer and ctx.Done became ready in the
	// same instant, and a select among ready cases picks at random. Half the
	// time a client that had run out of its own time was told the SERVER's wait
	// had expired. Both bounds still apply -- a client with five seconds left
	// does not get ninety -- but each is now reported by the thing that owns it.
	timer := time.NewTimer(queueWait)
	defer timer.Stop()

	select {
	case err := <-w.done:
		return err
	case <-ctx.Done():
		return r.giveUp(w, context.Cause(ctx))
	case <-timer.C:
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

// expiredWaitReason names both what happened and what blocked it.
//
// AN OPERATOR'S REMEDY DEPENDS ON WHICH CAP WAS IN THE WAY, so "you waited and
// were not served" is not a sufficient answer on its own: a full target pool
// and a full session cap are fixed by different changes, and the trail has to
// say which one held this request. This was found by an existing cell going
// red -- the queue had begun overwriting the specific cap identity with a
// generic timeout, which would have left an operator reading the trail with no
// idea which limit to raise.
//
// The returned error satisfies BOTH identities: the wait expired, AND the
// lease cap is why. The front door renders the specific cap to the client,
// while the waited-and-was-not-served fact stays available to anything reading
// for pressure.
func (r *sessionRegistry) expiredWaitReason(w *admitWaiter) error {
	r.mu.Lock()
	blocked := w.blockedBy
	r.mu.Unlock()
	if blocked == nil {
		return ErrQueueTimeout
	}
	return fmt.Errorf("%w: %w", ErrQueueTimeout, blocked)
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

// dropTargetWaiters refuses everyone waiting for one connection, because that
// connection is being removed.
//
// ANSWERED AT THE MOMENT OF REMOVAL, not by letting the wait expire. A timeout
// invites a retry that can now never succeed; this says the thing they asked
// for no longer exists. It is called under the same intent as marking the
// connection draining: a request may not be admitted onto a connection being
// torn down, and a request waiting for one is exactly that request a moment
// earlier.
func (r *sessionRegistry) dropTargetWaiters(connID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.line[:0]
	n := 0
	for _, w := range r.line {
		if w.leaseConn == connID {
			w.resolve(ErrTargetGone)
			n++
			continue
		}
		kept = append(kept, w)
	}
	r.line = kept
	return n
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

// clock is the registry's time source, injectable so a cell can drive the wait
// without sleeping through it.
func (r *sessionRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
