package exec

import (
	"errors"
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

// ErrTargetGone is a request that was waiting for a connection an operator
// removed while it waited.
//
// ANSWERED AT THE MOMENT OF REMOVAL, not by letting the wait expire. The two
// are not the same answer: a timeout tells the caller the instance was busy and
// invites a retry that can now never succeed, while this says the thing they
// asked for no longer exists. Leaving them to time out would also leave the
// queue able to grant a permit against a connection that is being torn down.
var ErrTargetGone = errors.New("exec: the connection this request was waiting for was removed")

// ErrEngineClosing is a request that was still in line when the instance began
// shutting down.
//
// WITHOUT THIS, SHUTDOWN IS SILENTLY SLOW AND THEN WRONG. A waiter with no
// deadline of its own would sit for the full ninety seconds after the engine
// stopped serving, and a release landing in that window would hand it a permit
// for a pool that is already closed. Waking every waiter at the start of
// shutdown makes the wait end when serving ends.
var ErrEngineClosing = errors.New("exec: the instance is shutting down")
