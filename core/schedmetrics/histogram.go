package schedmetrics

import "time"

// PER-SIGNAL EDGE SETS. THERE IS NO SHARED SET.
//
// An earlier specification said one set was "shared by every duration histogram",
// topping out at five minutes. That was right at a 90-second bound and is wrong
// now, and the connection-holding policy replaced it: each signal names its own
// set, and every boundary a signal exists to diagnose must be an actual edge —
// otherwise the interesting case lands in +Inf and the metric cannot answer
// the question it was built for.
//
// TWELVE EDGES EACH, SO THIRTEEN BINS EACH. The bin count is deliberately
// unchanged across both sets, because the test cell arithmetic depends on the
// COUNT and not on the values.
//
// These are transcribed from the ratified table, not chosen here. An earlier
// version of this file invented an eight-edge hold set and flagged it as a
// deviation needing a ruling; the ruling already existed and had been missed.

// QueueWaitEdgesMs resolves the 90-second queue deadline, which is the last
// edge rather than the overflow: a wait that reached its bound must be
// countable, and a bound sitting in +Inf is the one sample you cannot see.
var QueueWaitEdgesMs = []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 30000, 90_000}

// BackendHoldEdgesMs resolves the three holding bounds — 10 minutes, 2 hours
// and 8 hours — each of which is an exact edge.
//
// Reclamation and termination timing use this same set, per the same table.
var BackendHoldEdgesMs = []int64{
	1_000, 10_000, 60_000, 300_000,
	600_000,    // 10 min — session idle
	1_800_000,  // 30 min
	3_600_000,  // 1 h
	7_200_000,  // 2 h — idle in transaction
	14_400_000, // 4 h
	21_600_000, // 6 h
	28_800_000, // 8 h — the total transaction bound
	43_200_000, // 12 h — headroom above the bound, so a hold PAST it is still countable
}

// Histogram counts durations into exclusive internal bins.
//
// TWO CONVENTIONS, BOTH TRUE OF DIFFERENT THINGS, and conflating them makes a
// boundary trace unverifiable. INTERNALLY a sample increments exactly one bin
// — the lowest edge e with sample <= e. ON EXPORT bin i is the running sum of
// bins 0..i, which is the standard `le` form a scrape expects. Bins() and
// Cumulative() are separate methods for that reason, and the boundary cells
// assert both.
//
//	  Sample Landings:
//	  • Internal Storage (Bins()):
//	      Exclusive: sample <= edge increments exactly ONE bucket.
//	  • Export Projection (Cumulative()):
//	      Monotonic running sum: bin[i] = sum(0..i), final bin == Count().
//
//	  Sample: 45ms
//	  Edges: [10ms, 50ms, 100ms, +Inf]
//	  Internal:   [0, 1, 0, 0]
//	  Cumulative: [0, 1, 1, 1]
type Histogram struct {
	edges []int64
	bins  []uint64 // len(edges)+1; the last is the +Inf overflow
	count uint64
	sum   int64
}

// NewHistogram builds a histogram over edges, which must be ascending.
func NewHistogram(edges []int64) *Histogram {
	e := make([]int64, len(edges))
	copy(e, edges)
	return &Histogram{edges: e, bins: make([]uint64, len(e)+1)}
}

// Observe records one duration.
//
// A NEGATIVE SAMPLE IS REFUSED RATHER THAN CLAMPED. Every duration is measured
// between two events emitted by the same owner on one monotonic clock, so a
// negative one cannot happen — and if it does, the ordering assumption the
// whole signal rests on is broken. Folding it into the first bin would hide
// that; reporting false lets a caller's cell see it.
func (h *Histogram) Observe(d time.Duration) bool {
	ms := d.Milliseconds()
	if ms < 0 {
		return false
	}
	h.count++
	h.sum += ms
	for i, e := range h.edges {
		if ms <= e {
			h.bins[i]++
			return true
		}
	}
	h.bins[len(h.edges)]++ // +Inf
	return true
}

// Bins returns the EXCLUSIVE internal counts: each sample appears once.
func (h *Histogram) Bins() []uint64 {
	out := make([]uint64, len(h.bins))
	copy(out, h.bins)
	return out
}

// Cumulative returns the exported `le` form: bin i is the sum of bins 0..i,
// so the final entry equals Count.
func (h *Histogram) Cumulative() []uint64 {
	out := make([]uint64, len(h.bins))
	var running uint64
	for i, n := range h.bins {
		running += n
		out[i] = running
	}
	return out
}

// Count and Sum are the exact totals, kept independently of the bins so they
// survive a change of edges.
func (h *Histogram) Count() uint64 { return h.count }
func (h *Histogram) Sum() int64    { return h.sum }

// Edges is the boundary list, copied so a caller cannot reshape a live
// histogram's meaning after samples have landed in it.
func (h *Histogram) Edges() []int64 {
	out := make([]int64, len(h.edges))
	copy(out, h.edges)
	return out
}
