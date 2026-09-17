package pressure

import (
	"sort"
	"time"
)

// DenialKey is one row of the denial breakdown: what was refused, and who it
// was charged to.
//
// THE CHARGE IS PART OF THE KEY, NOT A LABEL BESIDE IT. The view's whole job
// during the incident would have been to let somebody see that the refusals
// were capacity and not credentials. Keying by reason alone and rendering the
// charge from a lookup would let the two drift the moment a reason is ever
// declared by a second producer with a different class -- and a row that says
// "capacity" about a credential refusal is the incident's mistake with a
// nicer font.
type DenialKey struct {
	Reason string
	Class  Class
}

// DenialBreakdown counts refusals per reason-and-class over the same sliding
// window as the rate signal.
//
// BOUNDED, BECAUSE REASONS ARE NOT AS FINITE AS THEY LOOK. The declared
// vocabulary is finite today, and this is keyed by a string that arrives from a
// producer at runtime; a future producer that derives a reason from anything
// the peer controls turns this map into the same memory-exhaustion primitive an
// unbounded source map would be. The cap costs nothing and removes a class of
// mistake that would otherwise depend on everybody remembering.
type DenialBreakdown struct {
	windows map[DenialKey]*rateWindow
	dim     *Dimension
}

// NewDenialBreakdown builds an empty breakdown.
func NewDenialBreakdown() *DenialBreakdown {
	return &DenialBreakdown{windows: map[DenialKey]*rateWindow{}, dim: NewDimension()}
}

// Add records one refusal.
func (b *DenialBreakdown) Add(k DenialKey, now time.Time) {
	if b.windows == nil {
		b.windows = map[DenialKey]*rateWindow{}
		b.dim = NewDimension()
	}
	// The dimension decides who is retained; the windows follow it, so the two
	// cannot disagree about which reasons exist.
	b.dim.Note(k.Reason+"\x00"+k.Class.String(), now)
	if b.windows[k] == nil {
		b.windows[k] = &rateWindow{}
	}
	b.windows[k].add(now)
	b.prune()
}

// DenialRow is one rendered row of the breakdown.
type DenialRow struct {
	DenialKey
	Count int
}

// Rows reports the breakdown for the window ending now, most refusals first
// and then by reason, so a view is stable between reads.
//
// ORDERED BY COUNT, BECAUSE THE VIEW IS READ TOP-DOWN. The operator's question
// is "what is refusing people", and the answer they need first is the one
// happening most. Ties fall back to the reason for the same total-order reason
// the dimension has one: without it the rows reshuffle on every read and the
// surface looks like it is flapping.
func (b *DenialBreakdown) Rows(now time.Time) []DenialRow {
	out := make([]DenialRow, 0, len(b.windows))
	for k, w := range b.windows {
		if n := w.total(now); n > 0 {
			out = append(out, DenialRow{DenialKey: k, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// Omitted reports how many reason-and-class rows have been dropped to stay
// within the cap.
func (b *DenialBreakdown) Omitted() int { return b.dim.Omitted() }

// prune drops windows for keys the dimension has evicted, so the two cannot
// disagree about which reasons exist.
func (b *DenialBreakdown) prune() {
	if len(b.windows) <= MaxSubjects {
		return
	}
	keep := map[string]bool{}
	for _, s := range b.dim.Subjects() {
		keep[s] = true
	}
	for k := range b.windows {
		if !keep[k.Reason+"\x00"+k.Class.String()] {
			delete(b.windows, k)
		}
	}
}
