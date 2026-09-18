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
	h := NewHistogram(QueueWaitEdgesMs)
	if h.Observe(-time.Second) {
		t.Fatal("a negative duration was accepted")
	}
	if h.Count() != 0 || h.Sum() != 0 {
		t.Errorf("a refused sample still moved the totals: count=%d sum=%d", h.Count(), h.Sum())
	}
}

// EVERY BOUND IS AN ACTUAL EDGE, AND A SAMPLE AT IT IS COUNTABLE.
//
// The ratified requirement, in its own words: every boundary a signal exists
// to diagnose must be an actual edge, otherwise the interesting case lands in
// +Inf and the metric cannot answer the question it was built for.
//
// So this does not check the numbers against a copy of the list — that would
// only prove the transcription. It drives a sample AT each bound and requires
// it to land in a NAMED bin, which is the property the operator depends on.
func TestHistogram_ASampleAtEachBoundIsCountable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edges []int64
		at    time.Duration
	}{
		{"the 90s queue deadline", QueueWaitEdgesMs, 90 * time.Second},
		{"the 10m session idle bound", BackendHoldEdgesMs, 10 * time.Minute},
		{"the 2h idle-in-transaction bound", BackendHoldEdgesMs, 2 * time.Hour},
		{"the 8h total transaction bound", BackendHoldEdgesMs, 8 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := tc.at.Milliseconds()

			// THE BOUND IS AN EDGE, EXACTLY. "not in +Inf" is too weak and a
			// control proved it: the hold set carries 12h of headroom above
			// the 8h bound, so deleting the 8h edge still leaves an 8h sample
			// countable — in the WRONG bin, lumped with everything up to 12h.
			// An operator asking "are holds piling up AT the bound" would read
			// that as fine.
			var idx = -1
			for i, e := range tc.edges {
				if e == ms {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("%v (%dms) is not an edge of this set; the ratified rule is that "+
					"every boundary a signal exists to diagnose must be an ACTUAL edge, so a "+
					"sample at the bound falls in with everything below the next one",
					tc.at, ms)
			}

			h := NewHistogram(tc.edges)
			if !h.Observe(tc.at) {
				t.Fatalf("%v was refused", tc.at)
			}
			bins := h.Bins()
			if bins[idx] != 1 {
				t.Errorf("a sample exactly at %v did not land in its own bin %d; `sample <= e` "+
					"puts a bound INTO the bin it names, which is what makes the bound "+
					"readable", tc.at, idx)
			}
			if overflow := bins[len(tc.edges)]; overflow != 0 {
				t.Errorf("a sample at %v landed in +Inf", tc.at)
			}
		})
	}
}

// AND THE TWO SETS EXIST BECAUSE ONE CANNOT DO BOTH JOBS.
//
// The queue set stops at the 90-second deadline by design. Measuring a hold
// with it puts every hold past that point in +Inf, which is what "no shared
// edge set" was ruled to prevent. This cell is the reason there are two sets
// rather than an observation about a mistake.
func TestHistogram_TheQueueSetCannotResolveHoldBounds(t *testing.T) {
	holds := []time.Duration{10 * time.Minute, 2 * time.Hour, 8 * time.Hour}

	queue := NewHistogram(QueueWaitEdgesMs)
	for _, d := range holds {
		queue.Observe(d)
	}
	if got := queue.Bins()[len(QueueWaitEdgesMs)]; got != uint64(len(holds)) {
		t.Fatalf("the queue set placed %d of %d hold bounds outside +Inf; if that has changed "+
			"the sets have converged and one of them is no longer the ratified schema", got, len(holds))
	}

	hold := NewHistogram(BackendHoldEdgesMs)
	for _, d := range holds {
		hold.Observe(d)
	}
	if got := distinctBins(hold.Bins()); got != len(holds) {
		t.Errorf("the hold set separated the three bounds into %d bins, want %d — an operator "+
			"asking which bound holds are piling against needs them apart", got, len(holds))
	}
	if got := hold.Bins()[len(BackendHoldEdgesMs)]; got != 0 {
		t.Errorf("%d of the three bounds landed in +Inf under the hold set", got)
	}
}

// TWELVE EDGES, THIRTEEN BINS, BOTH SETS.
//
// The bin COUNT is what the design's cell arithmetic depends on, explicitly
// not the edge values — so a set that changed its length would break
// arithmetic elsewhere while every value in it still looked reasonable.
func TestHistogram_BothRatifiedSetsAreTwelveEdges(t *testing.T) {
	for name, edges := range map[string][]int64{
		"queue_wait_ms":   QueueWaitEdgesMs,
		"backend_hold_ms": BackendHoldEdgesMs,
	} {
		if len(edges) != 12 {
			t.Errorf("%s has %d edges, want 12; the bin count is what the cell arithmetic "+
				"depends on", name, len(edges))
		}
		h := NewHistogram(edges)
		if got := len(h.Bins()); got != 13 {
			t.Errorf("%s yields %d bins, want 13", name, got)
		}
		for i := 1; i < len(edges); i++ {
			if edges[i] <= edges[i-1] {
				t.Errorf("%s edges are not ascending at %d (%d after %d)", name, i, edges[i], edges[i-1])
			}
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
