package pressure

import (
	"testing"
	"time"
)

// A BURST THAT SPANS TWO BUCKETS SUMS; IT DOES NOT RESET.
//
// This is the failure a naive ring buffer has and the reason each slot carries
// the absolute bucket it holds rather than just a count. Denials do not wait
// for a boundary, so the commonest real burst is one that straddles two -- and
// a window that reset on the crossing would report half of it and clear an
// alert that should have stayed up.
func TestWindow_ABurstAcrossABoundarySums(t *testing.T) {
	base := time.Unix(0, 0).Add(bucketSpan) // comfortably inside a bucket
	var w rateWindow

	// Three at the end of one bucket, three at the start of the next.
	for range 3 {
		w.add(base)
	}
	next := base.Add(bucketSpan)
	for range 3 {
		w.add(next)
	}
	if got := w.total(next); got != 6 {
		t.Errorf("a burst spanning two buckets totalled %d, want 6 — a window that resets "+
			"on a boundary reports a fraction of what happened and clears an alert early", got)
	}
}

// A DENIAL ON THE BOUNDARY IS COUNTED ONCE.
//
// Landing exactly on a bucket edge is the case where an off-by-one puts an
// event in two slots or in none, and it is the case nobody produces by hand.
func TestWindow_AnEventOnTheBoundaryIsCountedOnce(t *testing.T) {
	var w rateWindow
	edge := time.Unix(0, 0).Add(5 * bucketSpan) // exactly a boundary
	w.add(edge)
	if got := w.total(edge); got != 1 {
		t.Errorf("an event exactly on a boundary totalled %d, want 1", got)
	}
	if got := w.total(edge.Add(bucketSpan / 2)); got != 1 {
		t.Errorf("the same event totalled %d half a bucket later, want 1", got)
	}
}

// THE TOTAL FALLS TO ZERO ON ITS OWN, AND THE SLOTS ARE ACTUALLY RELEASED.
//
// Two separate claims, and I split them because asserting only the first let a
// broken build pass. The total is correct because the sum ignores slots outside
// the window, so it would return to zero even if nothing were ever evicted --
// which means a cell asserting the total alone proves nothing about eviction. I
// found that by removing the eviction and watching this cell still pass.
//
// The total matters to the operator: an alert that never clears is one people
// are trained to ignore. The eviction matters to the machine: slots left
// holding a burst from an hour ago are memory that never comes back on a
// long-lived daemon. So the second half asserts the storage, not the answer.
func TestWindow_ItReturnsToZeroWithoutAnyFurtherWrites(t *testing.T) {
	start := time.Unix(0, 0).Add(bucketSpan)
	var w rateWindow
	for range denialsEnter {
		w.add(start)
	}
	if got := w.total(start); got < denialsEnter {
		t.Fatalf("the window totalled %d immediately after %d denials", got, denialsEnter)
	}
	// One full window later, with nothing added in between.
	later := start.Add(Window + bucketSpan)
	if got := w.total(later); got != 0 {
		t.Errorf("the window still totalled %d a full window after the last denial, with "+
			"no further writes", got)
	}
	// AND THE SLOTS THEMSELVES ARE CLEAR. Reading is the only thing that has
	// happened since, so if this holds, the read is what released them.
	for i, c := range w.counts {
		if c != 0 {
			t.Errorf("slot %d still holds %d after the window passed and was read; "+
				"eviction that happens only on write never reclaims anything once "+
				"the denials stop, which on a long-lived daemon is the point at "+
				"which it matters", i, c)
		}
	}
}

// AND IT SLIDES RATHER THAN JUMPING.
//
// Six buckets exist so the window moves in steps small enough that a total does
// not fall off a cliff. A denial should leave the window roughly a window after
// it arrived, not at the end of some fixed minute.
func TestWindow_TheOldestBucketFallsOutFirst(t *testing.T) {
	start := time.Unix(0, 0).Add(bucketSpan)
	var w rateWindow
	for i := range buckets {
		w.add(start.Add(time.Duration(i) * bucketSpan))
	}
	at := start.Add(time.Duration(buckets-1) * bucketSpan)
	if got := w.total(at); got != buckets {
		t.Fatalf("totalled %d with one denial in each of %d buckets", got, buckets)
	}
	// One bucket later the oldest has aged out, and only that one.
	if got := w.total(at.Add(bucketSpan)); got != buckets-1 {
		t.Errorf("totalled %d one bucket later, want %d — the window must slide by a "+
			"bucket, not reset", got, buckets-1)
	}
}
