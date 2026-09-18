// Package schedmetrics carries the scheduler's measurement vocabulary.
//
// It is scope 5b of the front-door programme: the signals that describe the
// machinery the scheduler and the reclamation ladder build. This file is the
// first piece — the closed label set for how a wait ended — because every
// other signal is dimensioned by it and a label set that can drift makes the
// rest unverifiable.
package schedmetrics

import (
	"context"
	"errors"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// WaitOutcome is how one queued admission ended.
//
// A CLOSED SET WITH NO FALLBACK BUCKET, deliberately. The alternative an
// `unknown` arm offers is a series that silently absorbs every outcome
// somebody forgets to map, so the count stays plausible while the breakdown
// stops describing anything. ClassifyWait reports failure instead, and the
// guard cells turn that failure into a build-time one.
type WaitOutcome uint8

// THE PRODUCIBLE OUTCOMES, and only those.
//
// Each of these has a raise site in core/exec today. Two outcomes the design
// names are deliberately ABSENT — see the reserved block below — because a
// manifest that declares what nothing can raise is a broken runtime contract
// rather than harmless preparation.
const (
	// OutcomeUnknown is the zero value and is never a label. It exists so a
	// failed classification is representable without inventing a bucket.
	OutcomeUnknown WaitOutcome = iota

	// WaitGranted: the wait ended in an admission. err == nil.
	WaitGranted
	// WaitExpired: the queue deadline passed without reaching the head.
	WaitExpired
	// WaitTargetGone: the connection waited for was removed by an operator.
	WaitTargetGone
	// WaitShuttingDown: the instance began closing while the request was in line.
	WaitShuttingDown
	// WaitCancelledByRequest: the caller stopped waiting.
	WaitCancelledByRequest
	// WaitAllCapacityInTransaction: the pre-enqueue refusal — every connection
	// is held by a transaction inside its bounds, so queueing would wait for a
	// cap that no release is coming to clear.
	//
	// NOT IN THE ORIGINAL DESIGN'S LABEL LIST, and it has to be: it is a real
	// producer (scheduler.go's pre-enqueue check) and an outcome a developer
	// meets. A label set that omits a producible outcome is the same defect as
	// one that declares an impossible one, pointing the other way.
	WaitAllCapacityInTransaction
)

// NOT DECLARED, AND THIS IS THE FINDING THAT SHAPES THE REST OF SCOPE 5b.
//
// scheduler.go's durable-refusal path delivers an ARBITRARY error to the
// waiter — a refusal that will not clear by waiting, which is a real outcome a
// developer meets. It cannot be classified here. It is not a sentinel, so
// errors.Is cannot name it, and the only way an error-classifier could reach
// it is a trailing "anything else" arm — which is the fallback bucket the
// design forbids by name, because such an arm silently absorbs every sentinel
// somebody later adds without coming here.
//
// So the outcome is unreachable through this seam, and that is not a gap in
// this file: it is the reason the design says the labels must be GENERATED
// FROM THE TYPED RESULT rather than derived from the error. The typed result
// does not exist — the scheduler resolves waiters with sentinel errors — and
// this outcome is the proof that classification-by-error cannot substitute for
// it. Introducing that result in core/exec is scope 5b's real first task.

// RESERVED — normative, not declared at runtime.
//
// The design names two further outcomes that NOTHING IN core/exec CAN RAISE
// TODAY, so neither appears in the constants above and neither gets a label:
//
//   - Disconnected — a peer that went away, as distinct from a caller that
//     chose to stop waiting. Both arrive as a cancelled context and the
//     scheduler cannot tell them apart, so declaring the distinction would
//     assert a precision the code does not have.
//   - TargetChanged — a connection reconfigured under a waiting request. No
//     raise site exists.
//
// They are promoted into the set in the same change that adds their raise
// sites, never before. This mirrors the accepted rule that a production
// outcome manifest lists only conditions a current producer can raise, which
// the front-door work already applies to two reclaim rows.

// label is the metric label for each outcome. Kept beside the constants so a
// new outcome without a label is visible in one screen rather than two files.
var label = map[WaitOutcome]string{
	WaitGranted:                  "granted",
	WaitExpired:                  "expired",
	WaitTargetGone:               "target_gone",
	WaitShuttingDown:             "shutting_down",
	WaitCancelledByRequest:       "cancelled_by_request",
	WaitAllCapacityInTransaction: "all_capacity_in_transaction",
}

// Label renders the outcome. The zero value has no label, which is what makes
// an unclassified outcome impossible to report as a real one.
func (o WaitOutcome) Label() (string, bool) {
	s, ok := label[o]
	return s, ok
}

// ClassifyWait maps what a wait returned onto its outcome.
//
// IT REPORTS FAILURE RATHER THAN GUESSING. The second return is false for
// anything outside the closed set — including a new sentinel somebody adds to
// core/exec without coming here — so the caller cannot record a count under a
// label that does not describe it.
//
// Order matters only where sentinels nest; each arm below is a distinct
// errors.Is target today.
func ClassifyWait(err error) (WaitOutcome, bool) {
	switch {
	case err == nil:
		return WaitGranted, true
	case errors.Is(err, exec.ErrQueueTimeout):
		return WaitExpired, true
	case errors.Is(err, exec.ErrTargetGone):
		return WaitTargetGone, true
	case errors.Is(err, exec.ErrEngineClosing):
		return WaitShuttingDown, true
	case errors.Is(err, exec.ErrAllCapacityInTransaction):
		return WaitAllCapacityInTransaction, true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller stopped waiting. A peer that disconnected arrives here
		// too and is NOT separated — see the reserved block.
		return WaitCancelledByRequest, true
	}
	return OutcomeUnknown, false
}
