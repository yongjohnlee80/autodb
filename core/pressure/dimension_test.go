package pressure

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A DIMENSION KEYED BY SOMETHING THE PEER CHOOSES STAYS BOUNDED.
//
// Source addresses come from whoever dialled us. An unbounded map keyed by one
// is a memory exhaustion primitive handed to anybody who can open a socket, and
// it fills fastest during exactly the incident this view exists to explain.
func TestDimension_ItStaysBoundedUnderAFloodOfSubjects(t *testing.T) {
	d := NewDimension()
	base := time.Unix(0, 0)
	for i := range 500 {
		d.Note(fmt.Sprintf("10.0.0.%d", i), base.Add(time.Duration(i)*time.Second))
	}
	if d.Len() > MaxSubjects {
		t.Errorf("held %d subjects after 500 arrived, want at most %d", d.Len(), MaxSubjects)
	}
	if d.Omitted() != 500-MaxSubjects {
		t.Errorf("reported %d omitted, want %d — a list silently truncated tells an "+
			"operator that %d sources are throttled when 500 are",
			d.Omitted(), 500-MaxSubjects, MaxSubjects)
	}
}

// THE RECENT SURVIVE, NOT THE EARLY.
//
// The view answers "what is happening now". Evicting by first-seen would pin it
// to whoever arrived first and leave it describing a situation that has ended.
func TestDimension_TheLeastRecentIsDropped(t *testing.T) {
	d := NewDimension()
	base := time.Unix(0, 0)
	for i := range MaxSubjects + 4 {
		d.Note(fmt.Sprintf("s%02d", i), base.Add(time.Duration(i)*time.Second))
	}
	subs := d.Subjects()
	if len(subs) != MaxSubjects {
		t.Fatalf("held %d, want %d", len(subs), MaxSubjects)
	}
	if subs[0] != fmt.Sprintf("s%02d", MaxSubjects+3) {
		t.Errorf("most recent is %q, want the last one noted", subs[0])
	}
	for _, gone := range []string{"s00", "s01", "s02", "s03"} {
		for _, s := range subs {
			if s == gone {
				t.Errorf("%q survived although it was among the least recent", gone)
			}
		}
	}
}

// EQUAL TIMESTAMPS EVICT AND ORDER DETERMINISTICALLY.
//
// A tick stamps many subjects at once, so ties are the common case rather than
// the exotic one. Without a total order the survivors are chosen in Go's
// deliberately randomised map order: the view shows a different sixteen on each
// read, which reads as flapping to the person watching and cannot be asserted
// at all. Repetition is the point of this cell — a single pass can agree with
// map order by luck.
func TestDimension_TiesAreBrokenLexicographicallyAndRepeatably(t *testing.T) {
	at := time.Unix(0, 0)
	var first []string
	for round := range 20 {
		d := NewDimension()
		// All noted at the SAME instant, in an order that varies by round.
		for i := range MaxSubjects + 6 {
			j := (i + round) % (MaxSubjects + 6)
			d.Note(fmt.Sprintf("s%02d", j), at)
		}
		got := d.Subjects()
		if len(got) != MaxSubjects {
			t.Fatalf("round %d held %d, want %d", round, len(got), MaxSubjects)
		}
		if first == nil {
			first = got
			continue
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("round %d gave %v, round 0 gave %v — with every timestamp equal "+
					"the survivors and their order must not depend on map iteration",
					round, got, first)
			}
		}
	}
	// And the survivors are the lexicographically first, which is the stated rule.
	if first[0] != "s00" {
		t.Errorf("the tie-break kept %q first, want s00", first[0])
	}
}

// THE OMISSION IS RENDERED, NOT JUST COUNTED.
//
// "and N more" is the difference between a bounded view and a misleading one.
func TestDimension_TheSummaryNamesWhatItLeftOut(t *testing.T) {
	d := NewDimension()
	base := time.Unix(0, 0)
	for i := range 30 {
		d.Note(fmt.Sprintf("s%02d", i), base.Add(time.Duration(i)*time.Second))
	}
	got := d.Summary(3)
	if !strings.Contains(got, "and ") || !strings.Contains(got, " more") {
		t.Errorf("summary %q does not say how many it left out", got)
	}
	// 30 arrived, 16 retained, 3 shown: 27 unaccounted for.
	if !strings.Contains(got, fmt.Sprintf("and %d more", 30-3)) {
		t.Errorf("summary %q does not account for every subject that arrived; showing a "+
			"short list without the true remainder tells the operator the problem is "+
			"smaller than it is", got)
	}
}

// A SUBJECT NOBODY HAS SEEN SINCE THE WINDOW EXPIRES.
//
// A throttled source appears while it is throttled and disappears when its
// window ends; a view that keeps it teaches the operator that the view lies.
func TestDimension_StaleSubjectsAreDroppedOnTheTickThatReads(t *testing.T) {
	d := NewDimension()
	base := time.Unix(0, 0)
	d.Note("stale", base)
	d.Note("fresh", base.Add(30*time.Second))

	d.Expire(base.Add(40*time.Second), 20*time.Second)
	subs := d.Subjects()
	if len(subs) != 1 || subs[0] != "fresh" {
		t.Errorf("after expiry the dimension holds %v, want only the fresh subject", subs)
	}
	d.Expire(base.Add(90*time.Second), 20*time.Second)
	if d.Len() != 0 {
		t.Errorf("%d subjects survived a full expiry sweep; the totals must return to "+
			"zero or nothing bounds this on a long-lived daemon", d.Len())
	}
}
