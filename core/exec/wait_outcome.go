package exec

// HOW A QUEUED ADMISSION ENDED, AS A TYPE RATHER THAN AS AN ERROR.
//
// The measurement design requires the outcome labels be generated from a typed
// result rather than derived from the error, and gives the reason: the closed
// set had already drifted in the document whose job was to count it. Deriving
// them from errors cannot work here, and one path proves it — serveLine's
// dispatch resolves a waiter with whatever admitLocked returned, which is nil
// on success and an ARBITRARY policy refusal otherwise. No errors.Is arm can
// name that refusal, and the only way to reach it from an error is a trailing
// catch-all, which silently absorbs every sentinel added later while the total
// stays plausible.
//
// So the RAISE SITE names the outcome. That is the whole change: each site
// already knows which of these it is, and previously threw that knowledge away.
//
// IT IS A MEASUREMENT AND MAY NOT FEED A CONTROL. The design says so, and the
// shape enforces it: admitWithLeaseOrWait still returns only `error`, and the
// outcome reaches an observer. Nothing can branch on it without first adding a
// path for it to travel, which is a change a reviewer would see.
type WaitOutcome uint8

const (
	// WaitOutcomeUnset is the zero value. It is never reported: a resolved
	// wait always names one of the outcomes below, and a cell asserts that a
	// zero value never reaches the observer.
	WaitOutcomeUnset WaitOutcome = iota

	// WaitGranted: the wait ended in an admission.
	WaitGranted

	// WaitDurableRefusal: admission was refused for a reason that will not
	// clear by waiting — a per-user cap, a budget, a policy. Delivered to the
	// waiter rather than swallowed, because leaving the request in line would
	// hold capacity for a cap no release is coming to lift.
	//
	// NOT DialFailed. The design's list names a dial failure, and this is not
	// one: admitLocked is an admission-policy decision and the backend dial
	// happens later, on another seam. Reusing the label would file policy
	// refusals under a network fault.
	WaitDurableRefusal

	// WaitAllCapacityInTransaction: the pre-enqueue refusal. Every lease on
	// the target is held by a transaction inside its bounds, so no release is
	// pending and waiting would end ninety seconds later with the answer that
	// was available immediately.
	//
	// DECIDED BEFORE THE WAIT AND ONLY THERE, so a request that goes on to
	// wait never carries this outcome — a record claiming a request never
	// waited has to be true of that request.
	WaitAllCapacityInTransaction

	// WaitExpired: the queue deadline passed without reaching the head.
	WaitExpired

	// WaitTargetGone: the connection waited for was removed by an operator.
	WaitTargetGone

	// WaitShuttingDown: the instance began closing while the request was in
	// line, or had already begun when it arrived.
	WaitShuttingDown

	// WaitCancelledByRequest: the caller stopped waiting, by cancelling or by
	// running out of its own deadline.
	//
	// A PEER THAT VANISHED ARRIVES HERE TOO, and is not separated. The design
	// names a distinct Disconnected outcome; the scheduler cannot tell the two
	// apart — both are a cancelled context — so reporting the distinction
	// would assert a precision this code does not have. It is promoted when a
	// producer can raise it, not before.
	WaitCancelledByRequest
)

// waitResult is what travels to the caller: the outcome the raise site named,
// and the error the caller is owed. One value, so the two cannot be received
// out of step with each other.
type waitResult struct {
	outcome WaitOutcome
	err     error
}

// String is the outcome's stable identity. It is the ONLY place the spelling
// lives; the metrics label set is derived from it rather than restating it.
func (o WaitOutcome) String() string {
	switch o {
	case WaitGranted:
		return "granted"
	case WaitDurableRefusal:
		return "durable_refusal"
	case WaitAllCapacityInTransaction:
		return "all_capacity_in_transaction"
	case WaitExpired:
		return "expired"
	case WaitTargetGone:
		return "target_gone"
	case WaitShuttingDown:
		return "shutting_down"
	case WaitCancelledByRequest:
		return "cancelled_by_request"
	}
	return ""
}

// WaitOutcomes is every outcome a producer can raise, in declaration order.
//
// EXPORTED SO THE MEASUREMENT SIDE CAN BE TOTAL OVER IT rather than keeping a
// second list that drifts. A new outcome added above and not added here fails
// the completeness cell; one added here with no String() case fails it too.
func WaitOutcomes() []WaitOutcome {
	return []WaitOutcome{
		WaitGranted,
		WaitDurableRefusal,
		WaitAllCapacityInTransaction,
		WaitExpired,
		WaitTargetGone,
		WaitShuttingDown,
		WaitCancelledByRequest,
	}
}
