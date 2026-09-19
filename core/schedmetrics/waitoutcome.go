// Package schedmetrics carries the scheduler's measurement vocabulary.
//
// It is scope 5b of the front-door programme: the signals that describe the
// machinery the scheduler and the reclamation ladder build.
package schedmetrics

import "github.com/yongjohnlee80/autodb/core/exec"

// Label is the metric label for one wait outcome.
//
// DERIVED FROM THE TYPE, NOT RESTATED. The measurement design requires the
// labels be generated from the typed result, and gives the reason: a
// hand-written set had already drifted in the document whose job was to count
// it. So the spelling lives in exec.WaitOutcome.String() and this package owns
// no second copy of it — there is nothing here to drift.
//
// An earlier attempt classified the ERROR instead, and it could not work: the
// dispatch path resolves a waiter with nil on success and an arbitrary policy
// refusal otherwise, so naming that refusal needed a trailing catch-all, which
// is the fallback bucket the design forbids because it absorbs every sentinel
// added later while the total stays plausible. The raise sites now name their
// outcome and the problem is gone rather than worked around.
//
//	[core/exec.WaitOutcome Enum]
//	               │
//	               ├─ o == WaitOutcomeUnset ───> ("", false) [Suppressed]
//	               │
//	               └─ o != WaitOutcomeUnset ───> (o.String(), true) [Exported]
//	                  (e.g., "acquired", "queue_timeout", "canceled")
//
// The second return is false only for the zero value, which is never reported.
func Label(o exec.WaitOutcome) (string, bool) {
	s := o.String()
	return s, s != ""
}

// WaitLabels is every label the wait-outcome dimension can take, in the
// order the outcomes are declared.
//
// NO FALLBACK MEMBER. A series that absorbs unmapped outcomes keeps a
// plausible total while the breakdown stops describing anything, and an
// operator reads the two as equally trustworthy.
func WaitLabels() []string {
	outs := exec.WaitOutcomes()
	labels := make([]string, 0, len(outs))
	for _, o := range outs {
		if l, ok := Label(o); ok {
			labels = append(labels, l)
		}
	}
	return labels
}
