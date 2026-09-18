package schedmetrics

import (
	"testing"
	"time"
)

// THE BOUNDARY TRACE, ASSERTING BOTH CONVENTIONS.
//
// Internally a sample increments exactly ONE bin — the lowest edge e with
// sample <= e. On export bin i is the running sum of 0..i. Both are true of
// different things, and a cell that checks only one cannot tell a correct
// histogram from one that double-counts: the cumulative view of a
// double-counted set still rises monotonically and still ends at a plausible
// number.
func TestHistogram_ExclusiveInternallyCumulativeOnExport(t *testing.T) {
	h := NewHistogram([]int64{10, 100, 1000})

	// One sample exactly ON an edge belongs to that edge: `sample <= e`.
	for _, ms := range []int64{10, 10, 100, 5000} {
		if !h.Observe(time.Duration(ms) * time.Millisecond) {
			t.Fatalf("%dms was refused", ms)
		}
	}

	bins := h.Bins()
	want := []uint64{2, 1, 0, 1} // <=10: two; <=100: one; <=1000: none; +Inf: one
	for i := range want {
		if bins[i] != want[i] {
			t.Errorf("internal bin %d = %d, want %d (exclusive: each sample once)", i, bins[i], want[i])
		}
	}
	if sum := bins[0] + bins[1] + bins[2] + bins[3]; uint64(sum) != h.Count() {
		t.Errorf("the exclusive bins total %d but Count is %d; a sample has been counted "+
			"twice or lost", sum, h.Count())
	}

	cum := h.Cumulative()
	wantCum := []uint64{2, 3, 3, 4}
	for i := range wantCum {
		if cum[i] != wantCum[i] {
			t.Errorf("cumulative bin %d = %d, want %d (le form)", i, cum[i], wantCum[i])
		}
	}
	if cum[len(cum)-1] != h.Count() {
		t.Errorf("the last cumulative bin is %d and Count is %d; the export must end at the "+
			"total or a scrape reads a different population than _count reports",
			cum[len(cum)-1], h.Count())
	}
	if h.Sum() != 10+10+100+5000 {
		t.Errorf("Sum = %d, want 5120; the exact total is kept apart from the bins so it "+
			"survives a change of edges", h.Sum())
	}
}

// A NEGATIVE DURATION IS REFUSED, NOT CLAMPED.
//
// It cannot happen — both ends come from one monotonic clock and the same
// owner — so if it does, the ordering assumption every duration signal rests
// on has broken. Folding it into the first bin would hide that.
func TestHistogram_ANegativeSampleIsRefused(t *testing.T) {
	h := NewHistogram(LatencyEdgesMs)
	if h.Observe(-time.Second) {
		t.Fatal("a negative duration was accepted")
	}
	if h.Count() != 0 || h.Sum() != 0 {
		t.Errorf("a refused sample still moved the totals: count=%d sum=%d", h.Count(), h.Sum())
	}
}

// THE SHARED EDGES CANNOT MEASURE A HOLD, WHICH IS WHY HOLDS HAVE THEIR OWN.
//
// This is the finding the connection-holding ruling recorded and nobody
// applied. With the shared edges every realistic hold lands in the overflow
// bin, so the histogram reports that holds happen and says nothing about how
// long they last — the only question it exists to answer.
func TestHistogram_HoldEdgesDistinguishWhatLatencyEdgesCannot(t *testing.T) {
	holds := []time.Duration{
		2 * time.Minute,  // past the queue deadline
		30 * time.Minute, // past session idle
		3 * time.Hour,    // past idle-in-transaction
		9 * time.Hour,    // past the total bound: genuinely overflow
	}

	// THE CLAIM IS SEPARATION, NOT OVERFLOW. A first version of this cell
	// asserted all four land in overflow, which is false: two minutes fits
	// under the shared 300000ms top edge. The defect is not where they land
	// but that the shared edges CANNOT TELL THEM APART — three of these four
	// durations share one bin, so a reader cannot distinguish a half-hour hold
	// from a nine-hour one.
	shared := NewHistogram(LatencyEdgesMs)
	for _, d := range holds {
		shared.Observe(d)
	}
	if got := distinctBins(shared.Bins()); got > 2 {
		t.Fatalf("the shared edges separated these holds into %d bins; this cell exists "+
			"because they cannot separate them, and if that has changed the edges have moved "+
			"and HoldEdgesMs may no longer be needed", got)
	}

	hold := NewHistogram(HoldEdgesMs)
	for _, d := range holds {
		hold.Observe(d)
	}
	bins := hold.Bins()
	nonEmpty := distinctBins(bins)
	if nonEmpty < 4 {
		t.Errorf("the hold edges separated these four durations into %d bins, want 4; an "+
			"operator asking whether holds are piling up against a bound needs them apart",
			nonEmpty)
	}
	if got := bins[len(HoldEdgesMs)]; got != 1 {
		t.Errorf("overflow holds %d samples, want 1 — only the 9h hold is past the 8h bound", got)
	}
}

// THE LONG EDGES ARE THE RULED BOUNDS, EXACTLY.
//
// The register requires 90s/10m/2h/8h. A bin boundary sitting exactly on each
// bound is what lets an operator read "piling up against the bound" directly
// instead of doing arithmetic against numbers kept somewhere else.
func TestHistogram_HoldEdgesCarryTheRuledBounds(t *testing.T) {
	for _, want := range []int64{90_000, 600_000, 7_200_000, 28_800_000} {
		var found bool
		for _, e := range HoldEdgesMs {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%dms is not an edge; the ruled bounds are 90s, 10m, 2h and 8h and each "+
				"must be a boundary", want)
		}
	}
}

// distinctBins counts how many bins hold at least one sample — the measure of
// whether a set of edges can tell a group of durations apart at all.
func distinctBins(bins []uint64) int {
	var n int
	for _, c := range bins {
		if c > 0 {
			n++
		}
	}
	return n
}
