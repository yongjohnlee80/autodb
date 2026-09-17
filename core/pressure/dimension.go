package pressure

import (
	"fmt"
	"sort"
	"time"
)

// MaxSubjects is how many subjects any one keyed dimension will hold.
//
// EVERY KEYED DIMENSION IS ATTACKER-FACING, WHICH IS WHY THERE IS A CAP AT ALL.
// Users and targets are ours and bounded by configuration, but SOURCES are
// whoever dialled us and REASONS are bounded only by the vocabulary -- and a
// source address is chosen by the peer. An unbounded map keyed by source is a
// memory exhaustion primitive handed to anybody who can open a socket, and it
// would fill fastest during exactly the incident the view exists to explain.
//
// Sixteen is chosen to be more than an operator can read at once and far less
// than a machine notices.
const MaxSubjects = 16

// Dimension is a bounded set of subjects with the last time each was seen.
//
// RECENCY DECIDES WHO STAYS, because the question the view answers is "what is
// happening now". Dropping the least-recently-seen keeps the set converging on
// whoever is actually active, where dropping by first-seen would pin the view
// to whoever arrived first and leave it describing a situation that has ended.
//
// TIES ARE BROKEN LEXICOGRAPHICALLY, and that is not fussiness: without a total
// order, two subjects sharing a timestamp -- which is common, since a tick
// stamps many at once -- evict in map order, and map order in Go is
// deliberately random. The view would then show a different sixteen on each
// read, which reads as flapping to the person watching it and is untestable
// besides.
type Dimension struct {
	seen    map[string]time.Time
	omitted int
}

// NewDimension builds an empty dimension.
func NewDimension() *Dimension { return &Dimension{seen: map[string]time.Time{}} }

// Note records that a subject was seen now, evicting if that puts it over the
// cap.
func (d *Dimension) Note(subject string, now time.Time) {
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	d.seen[subject] = now
	d.evict()
}

// Expire drops every subject not seen within ttl of now.
//
// CALLED ON THE TICK THAT READS, not on a timer. A dimension nobody is looking
// at does not need tidying, and a timer to tidy it would be a goroutine per
// dimension doing nothing useful most of the time.
func (d *Dimension) Expire(now time.Time, ttl time.Duration) {
	for s, at := range d.seen {
		if now.Sub(at) > ttl {
			delete(d.seen, s)
		}
	}
}

// Subjects returns the retained subjects, most recent first, with a stable
// order among equal timestamps.
func (d *Dimension) Subjects() []string {
	out := make([]string, 0, len(d.seen))
	for s := range d.seen {
		out = append(out, s)
	}
	d.order(out)
	return out
}

// Len reports how many subjects are retained.
func (d *Dimension) Len() int { return len(d.seen) }

// Omitted reports how many subjects have been dropped to stay within the cap
// since the last time the count was read.
//
// THE NUMBER IS PART OF THE VIEW, not an implementation detail. A list silently
// truncated at sixteen tells an operator that sixteen sources are throttled
// when four hundred are, and that is a worse answer than refusing to show the
// list at all. "and N more" is the difference between a bounded view and a
// misleading one.
func (d *Dimension) Omitted() int { return d.omitted }

// Summary renders the retained subjects with the omission count appended, for
// a view or an event detail.
func (d *Dimension) Summary(limit int) string {
	subs := d.Subjects()
	if limit > 0 && len(subs) > limit {
		subs = subs[:limit]
	}
	out := ""
	for i, s := range subs {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	more := d.omitted + (d.Len() - len(subs))
	if more > 0 {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("and %d more", more)
	}
	return out
}

func (d *Dimension) evict() {
	for len(d.seen) > MaxSubjects {
		all := make([]string, 0, len(d.seen))
		for s := range d.seen {
			all = append(all, s)
		}
		d.order(all)
		// The last in order is the least recent, with the lexicographic
		// tie-break already applied.
		delete(d.seen, all[len(all)-1])
		d.omitted++
	}
}

// order sorts most-recent first, then lexicographically.
func (d *Dimension) order(subs []string) {
	sort.Slice(subs, func(i, j int) bool {
		a, b := d.seen[subs[i]], d.seen[subs[j]]
		if !a.Equal(b) {
			return a.After(b)
		}
		return subs[i] < subs[j]
	})
}
