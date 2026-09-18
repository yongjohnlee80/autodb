package schedmetrics

import "time"

// LatencyEdgesMs are the shared duration-histogram edges: 12 edges, and with
// the overflow, 13 exclusive internal bins.
//
// Fixed edges rather than sample lists, so exact totals survive eviction and
// bucket choice — which is also why _count and _sum accompany every histogram
// rather than being derived from the bins.
var LatencyEdgesMs = []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 30000, 300000}

// HoldEdgesMs are the edges for how long a backend was HELD, which is a
// different domain and needs different edges.
//
// THE SHARED EDGES TOP OUT AT FIVE MINUTES AND A HOLD IS BOUNDED AT EIGHT
// HOURS. Using them would put essentially every hold in the overflow bin: the
// histogram would report that holds happen and say nothing whatever about how
// long they last, which is the only question it exists to answer. The
// connection-holding ruling flagged exactly this and the fix was never
// applied.
//
// THE FOUR LONG EDGES ARE THE RULED BOUNDS THEMSELVES — 90s the queue
// deadline, 10m the session idle timeout, 2h idle-in-transaction, 8h the total
// transaction bound. That is deliberate: an operator reading this histogram is
// asking "are holds piling up against a bound", and a bin boundary that sits
// exactly on each bound answers it directly instead of requiring arithmetic
// against numbers kept somewhere else.
//
// A DEVIATION FROM THE MEASUREMENT DESIGN'S "shared by every duration
// histogram", recorded rather than quietly taken: the design predates the
// holding ruling that moved these bounds from 90s/5m to 2h/8h.
var HoldEdgesMs = []int64{
	100, 1000, 5000, 30000,
	90_000,     // the queue deadline
	600_000,    // session idle
	7_200_000,  // idle in transaction
	28_800_000, // the total transaction bound
}

// Histogram counts durations into exclusive internal bins.
//
// TWO CONVENTIONS, BOTH TRUE OF DIFFERENT THINGS, and conflating them makes a
// boundary trace unverifiable. INTERNALLY a sample increments exactly one bin
// — the lowest edge e with sample <= e. ON EXPORT bin i is the running sum of
// bins 0..i, which is the standard `le` form a scrape expects. Bins() and
// Cumulative() are separate methods for that reason, and the boundary cells
// assert both.
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
