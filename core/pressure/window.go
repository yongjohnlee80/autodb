package pressure

import "time"

const (
	// denialsEnter is the absolute count that raises the denial rate signal.
	//
	// AN ABSOLUTE COUNT, BECAUSE A RATE HAS NO DENOMINATOR, and five is chosen
	// so that "it would have fired early in the incident" is an assertion
	// somebody can run rather than a hope. The incident produced far more than
	// five capacity denials in a minute before anybody noticed anything.
	denialsEnter = 5

	// Window is how far back the denial rate looks.
	Window = time.Minute

	// buckets divides the window so it can move without a list of timestamps.
	//
	// SIX FIXED BUCKETS, NOT A SLICE THAT GROWS. Keeping one entry per denial
	// makes the memory a function of how badly things are going, which is the
	// worst possible time to start allocating -- a burst would cost most
	// exactly when the front door has least to spare. Six is coarse enough to
	// be free and fine enough that the window slides rather than jumping.
	buckets = 6
)

// bucketSpan is how long each bucket covers.
const bucketSpan = Window / buckets

// rateWindow counts events over a sliding window in fixed buckets.
//
// IT EVICTS ON READ AS WELL AS ON WRITE, which matters more than it looks: a
// denial signal that stopped being fed would otherwise keep reporting its last
// total forever, and an alert that never clears is one people are trained to
// ignore. Every method that consults the window advances it first, so a total
// is always about the window ending now.
type rateWindow struct {
	counts [buckets]int
	// at is the bucket index each slot currently holds, so a slot that belongs
	// to a window long past is recognised rather than added to. Storing the
	// index rather than a timestamp is what makes a burst that spans two
	// buckets SUM instead of resetting: both slots are current, so both count.
	at [buckets]int64
}

// index is the absolute bucket number for an instant. Absolute rather than
// modular so that two instants an entire window apart never share a slot
// identity, which is the bug a plain ring buffer has.
func index(t time.Time) int64 { return t.UnixNano() / int64(bucketSpan) }

// advance discards every slot that no longer belongs to the window ending now.
func (w *rateWindow) advance(now time.Time) {
	cur := index(now)
	oldest := cur - buckets + 1
	for i := range w.counts {
		if w.at[i] < oldest {
			w.counts[i] = 0
			w.at[i] = 0
		}
	}
}

// add records one event at this instant.
//
// IT DOES NOT ADVANCE THE WHOLE WINDOW, and it used to. The reset below covers
// the only slot add touches, and every reader advances before it sums -- so the
// extra sweep was work that changed no answer. Proved by deleting it and
// watching every cell still pass, which is also how it should have been
// justified in the first place.
func (w *rateWindow) add(now time.Time) {
	cur := index(now)
	slot := int(cur % buckets)
	if w.at[slot] != cur {
		// The slot belonged to an earlier window: it is reused, not added to.
		w.counts[slot] = 0
		w.at[slot] = cur
	}
	w.counts[slot]++
}

// total reports how many events fall in the window ending now.
func (w *rateWindow) total(now time.Time) int {
	w.advance(now)
	cur := index(now)
	oldest := cur - buckets + 1
	sum := 0
	for i := range w.counts {
		if w.at[i] >= oldest && w.at[i] <= cur {
			sum += w.counts[i]
		}
	}
	return sum
}

// DenialWindow is the exported face of the six-bucket rate window, so the front
// door can count refusals without this package owning the front door's state.
type DenialWindow struct{ w rateWindow }

// Add records one denial at this instant.
func (d *DenialWindow) Add(now time.Time) { d.w.add(now) }

// Total reports the denials inside the window ending now, evicting as it reads.
func (d *DenialWindow) Total(now time.Time) int { return d.w.total(now) }
