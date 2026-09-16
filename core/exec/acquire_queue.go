package exec

import (
	"context"
	"errors"
	"sync"
	"time"
)

// A REQUEST THAT CANNOT HAVE A CONNECTION NOW WAITS ITS TURN, AND THE TURN IS
// THE ORDER IT ASKED IN.
//
// Without a queue, a saturated target serves whoever happens to retry at the
// right microsecond. That is not a bound, it is a lottery: a developer who
// asked first can lose repeatedly to one who asks in a tight loop, and no
// amount of capacity fixes it because the unfairness is in the scheduling
// rather than in the supply. The incident this whole body of work started from
// was a capacity failure answered as a credential failure; the queue is the
// other half of that story, because it is what makes "wait" a real answer
// instead of "try again and hope".
//
// THE ORDER IS GLOBAL, NOT PER TARGET, and that is a decision rather than an
// accident of implementation. Per-target queues alone let a busy target starve
// a quiet one whenever they share the instance budget: the quiet target's
// waiter is at the head of an empty queue and still never runs, because every
// released permit is taken by the busy queue's head. One order over all
// waiters makes "oldest asked, first served" true of the instance rather than
// of each target separately, which is the property an operator can reason
// about. Per-target arrival order falls out of it for free.

// queueWait is how long a request may wait for a connection before it is told
// no.
//
// NINETY SECONDS, AND DELIBERATELY NOT DERIVED FROM THE TRANSACTION BOUNDS. An
// earlier design computed this from IdleInTxTimeout, which was 90s at the time
// and is now two hours. Two hours is how long a transaction may sit idle
// before it is reclaimed; it is not how long a person or a service should wait
// for an answer. Tying the two together meant a policy change about
// transaction tolerance silently became a policy change about request latency.
// This is its own fixed server policy, and it is not a developer knob.
const queueWait = 90 * time.Second

// ErrQueueTimeout is a request that waited its turn and never reached the head
// in time.
//
// SEPARATE FROM EVERY CAPACITY REFUSAL, because the two say different things to
// an operator. A capacity refusal means the instance had nothing to give and
// said so immediately. This means the instance took the request seriously,
// queued it, and could not serve it within the wait — which is a pressure
// signal rather than a rejection.
var ErrQueueTimeout = errors.New("exec: no connection became available within the queue wait")

// ErrAllCapacityInTransaction is the pre-enqueue refusal: every connection this
// instance can hand out is held by a transaction that is still within its
// bounds, so nothing is reclaimable now.
//
// PRE-ENQUEUE ONLY, AND THE WORD "ONLY" IS THE CONTRACT. A request already in
// the queue is never relabelled with this identity, even if the instance later
// enters that state. Once a request is queued it resolves exactly three ways —
// granted, the caller gave up, or the wait expired — and an occurrence
// claiming the request never entered the queue must be true of that request.
// Relabelling would put a "we refused you immediately" record against a
// request that in fact waited, which is a lie to whoever reads the trail.
var ErrAllCapacityInTransaction = errors.New("exec: every connection is held by a transaction within its bounds")

// waiter is one request's place in line.
type waiter struct {
	connID int64
	seq    uint64
	// ready carries the granted permit. Buffered by one so a granting
	// releaser never blocks on a waiter that has already given up: the send
	// completes, and the abandoned permit is recovered by the waiter's own
	// cleanup rather than by the releaser having to care.
	ready chan *Permit
}

// acquireQueue orders the waiters for one instance's connection budget.
type acquireQueue struct {
	mu   sync.Mutex
	seq  uint64
	line []*waiter // ordered oldest-first; the head is next to be served

	// now is the clock, injectable so a cell can drive the wait deterministically
	// rather than by sleeping.
	now func() time.Time
}

func newAcquireQueue() *acquireQueue {
	return &acquireQueue{now: time.Now}
}

// Depth reports how many requests are waiting, for the pressure view.
func (q *acquireQueue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.line)
}

// DepthFor reports how many of them are waiting on one connection.
func (q *acquireQueue) DepthFor(connID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, w := range q.line {
		if w.connID == connID {
			n++
		}
	}
	return n
}

// Wait puts the caller in line and blocks until it is served, gives up, or the
// wait expires.
//
// THE DEADLINE IS THE EARLIER OF THE TWO. A caller with five seconds left does
// not get ninety; a caller with no deadline gets exactly ninety. Taking the
// server's wait alone would hold a request long after its caller stopped
// caring, and taking the caller's alone would let one caller occupy a queue
// slot indefinitely.
func (q *acquireQueue) Wait(ctx context.Context, connID int64) (*Permit, error) {
	w := &waiter{connID: connID, ready: make(chan *Permit, 1)}

	q.mu.Lock()
	q.seq++
	w.seq = q.seq
	q.line = append(q.line, w)
	q.mu.Unlock()

	deadline := q.now().Add(queueWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case p := <-w.ready:
		return p, nil
	case <-ctx.Done():
		q.abandon(w)
		return nil, context.Cause(ctx)
	case <-timer.C:
		q.abandon(w)
		// THE RACE IS RESOLVED IN THE CALLER'S FAVOUR. A grant that landed in
		// the same instant the timer fired is a grant: the permit exists and
		// somebody paid for it, so returning a timeout here would strand a
		// live permit and tell a served caller it was not served.
		select {
		case p := <-w.ready:
			return p, nil
		default:
		}
		return nil, ErrQueueTimeout
	}
}

// abandon removes a waiter from the line.
func (q *acquireQueue) abandon(w *waiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.line {
		if x == w {
			q.line = append(q.line[:i], q.line[i+1:]...)
			return
		}
	}
}

// Grant hands a permit to the oldest waiter and reports whether anyone took it.
//
// CALLED WHEN A PERMIT IS RELEASED, not on a timer. A queue drained by polling
// adds latency to every grant and hides the moment capacity actually appeared;
// handing it over at the release point makes "oldest granted after physical
// release" literally what the code does.
func (q *acquireQueue) Grant(p *Permit) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.line) == 0 {
		return false
	}
	w := q.line[0]
	q.line = q.line[1:]
	w.ready <- p
	return true
}
