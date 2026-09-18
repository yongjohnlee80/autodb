package schedmetrics

import "fmt"

// RESET AND DISPOSITION ARE TWO AXES, NOT ONE LIST.
//
// An earlier design listed clean | failed | discarded together, and those
// overlap: a failed reset usually CAUSES a discard, so one event incremented
// two conceptually different things and the totals could not reconcile. Split,
// each axis answers one question — was a reset attempted and did it work, and
// where did the backend go — and the two reconcile by construction.

// ResetResult is how a reset attempt ended. Recorded ONLY when a reset was
// actually attempted, which is what makes the reconciliation below an
// inequality rather than an equality.
type ResetResult uint8

const (
	ResetResultUnset ResetResult = iota
	ResetClean
	ResetFailed
)

func (r ResetResult) String() string {
	switch r {
	case ResetClean:
		return "clean"
	case ResetFailed:
		return "failed"
	}
	return ""
}

// BackendDisposition is where a backend went. Recorded exactly ONCE per
// release, whether or not a reset ran.
type BackendDisposition uint8

const (
	DispositionUnset BackendDisposition = iota
	DispositionPooled
	DispositionDiscarded
	DispositionClosed
)

func (d BackendDisposition) String() string {
	switch d {
	case DispositionPooled:
		return "pooled"
	case DispositionDiscarded:
		return "discarded"
	case DispositionClosed:
		return "closed"
	}
	return ""
}

// DiscardReason is why a backend was discarded rather than pooled. A bounded
// set: a reason outside it is a defect, not a new member.
type DiscardReason uint8

const (
	DiscardReasonUnset DiscardReason = iota
	DiscardResetFailed
	DiscardResetTimeout
	DiscardNonIdleStatus
	DiscardDialFailed
	DiscardTargetChanged
	DiscardShutdown
)

func (d DiscardReason) String() string {
	switch d {
	case DiscardResetFailed:
		return "reset_failed"
	case DiscardResetTimeout:
		return "reset_timeout"
	case DiscardNonIdleStatus:
		return "non_idle_status"
	case DiscardDialFailed:
		return "dial_failed"
	case DiscardTargetChanged:
		return "target_changed"
	case DiscardShutdown:
		return "shutdown"
	}
	return ""
}

// ResetResults, Dispositions and DiscardReasons enumerate each bounded set, so
// the export side is total over them instead of keeping a second list.
func ResetResults() []ResetResult { return []ResetResult{ResetClean, ResetFailed} }
func Dispositions() []BackendDisposition {
	return []BackendDisposition{DispositionPooled, DispositionDiscarded, DispositionClosed}
}
func DiscardReasons() []DiscardReason {
	return []DiscardReason{
		DiscardResetFailed, DiscardResetTimeout, DiscardNonIdleStatus,
		DiscardDialFailed, DiscardTargetChanged, DiscardShutdown,
	}
}

// DispositionCounts holds the three counters for one target.
//
// DISCARD WITHOUT A RESET IS REPRESENTABLE, and has to be: a backend abandoned
// because its target changed is discarded with a reason and NO reset was ever
// attempted, so it must not touch the reset counter at all. That is the case
// the merged single-list design could not express.
type DispositionCounts struct {
	Reset    map[ResetResult]uint64
	Where    map[BackendDisposition]uint64
	Discards map[DiscardReason]uint64
}

func NewDispositionCounts() *DispositionCounts {
	return &DispositionCounts{
		Reset:    map[ResetResult]uint64{},
		Where:    map[BackendDisposition]uint64{},
		Discards: map[DiscardReason]uint64{},
	}
}

// NoteReset records that a reset was attempted and how it ended.
func (c *DispositionCounts) NoteReset(r ResetResult) error {
	if r.String() == "" {
		return fmt.Errorf("schedmetrics: reset result %d is outside the bounded set", r)
	}
	c.Reset[r]++
	return nil
}

// NoteRelease records where one backend went, and why if it was discarded.
//
// THE REASON IS REQUIRED FOR A DISCARD AND REFUSED OTHERWISE. A discard with
// no reason breaks the reconciliation below; a reason attached to a pooled
// backend asserts something that did not happen.
func (c *DispositionCounts) NoteRelease(d BackendDisposition, why DiscardReason) error {
	if d.String() == "" {
		return fmt.Errorf("schedmetrics: disposition %d is outside the bounded set", d)
	}
	switch {
	case d == DispositionDiscarded && why.String() == "":
		return fmt.Errorf("schedmetrics: a discard needs a reason from the bounded set, got %d", why)
	case d != DispositionDiscarded && why != DiscardReasonUnset:
		return fmt.Errorf("schedmetrics: disposition %s carries discard reason %s, which did not happen",
			d, why)
	}
	c.Where[d]++
	if d == DispositionDiscarded {
		c.Discards[why]++
	}
	return nil
}

// Reconcile checks the two invariants the split exists to make checkable.
//
// A reset is only counted when one was attempted, so resets cannot exceed
// releases; and every discard has exactly one reason, so the discard column
// and the reason total are the same number. Both are cheap and both catch a
// miscount at the moment it happens rather than in a dashboard weeks later.
func (c *DispositionCounts) Reconcile() error {
	var resets, releases, discards, reasons uint64
	for _, n := range c.Reset {
		resets += n
	}
	for _, n := range c.Where {
		releases += n
	}
	discards = c.Where[DispositionDiscarded]
	for _, n := range c.Discards {
		reasons += n
	}
	if resets > releases {
		return fmt.Errorf("schedmetrics: %d reset results for %d releases; a reset is only "+
			"recorded when one was attempted, so it cannot outnumber the releases it "+
			"belongs to", resets, releases)
	}
	if discards != reasons {
		return fmt.Errorf("schedmetrics: %d discards but %d discard reasons; every discard "+
			"has exactly one reason, so a difference means one of the two counters is being "+
			"written without the other", discards, reasons)
	}
	return nil
}
