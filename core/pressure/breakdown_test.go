package pressure

import (
	"fmt"
	"testing"
	"time"
)

// THE BREAKDOWN SEPARATES CAPACITY FROM CREDENTIALS, WHICH IS THE WHOLE POINT.
//
// During the incident, refusals were happening and nobody could see what KIND.
// A view that showed a single denial total would have been read as "somebody is
// hammering us" when the truth was "we have run out of room" — and the response
// to those two is opposite. The class is part of the key so the two can never be
// summed into one row.
func TestBreakdown_CapacityAndCredentialsAreSeparateRows(t *testing.T) {
	at := time.Unix(0, 0).Add(bucketSpan)
	b := NewDenialBreakdown()

	for range 7 {
		b.Add(DenialKey{Reason: "frontdoor/lease-cap-exceeded", Class: Capacity}, at)
	}
	for range 2 {
		b.Add(DenialKey{Reason: "frontdoor/denied", Class: Credential}, at)
	}

	rows := b.Rows(at)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — capacity and credential refusals must never be "+
			"summed into one figure, because the response to each is the opposite", len(rows))
	}
	// Most refusals first: the operator reads top-down and needs the biggest first.
	if rows[0].Count != 7 || rows[0].Class != Capacity {
		t.Errorf("first row is %+v, want the 7 capacity refusals", rows[0])
	}
	if rows[1].Count != 2 || rows[1].Class != Credential {
		t.Errorf("second row is %+v, want the 2 credential refusals", rows[1])
	}
}

// THE SAME REASON UNDER TWO CLASSES IS TWO ROWS.
//
// Keying by reason alone and rendering the class from a lookup would let the two
// drift the moment a reason is declared by a second producer with a different
// class — and a row labelled "capacity" about a credential refusal is the
// incident's own mistake in a nicer font.
func TestBreakdown_OneReasonUnderTwoClassesDoesNotMerge(t *testing.T) {
	at := time.Unix(0, 0).Add(bucketSpan)
	b := NewDenialBreakdown()
	b.Add(DenialKey{Reason: "shared/reason", Class: Capacity}, at)
	b.Add(DenialKey{Reason: "shared/reason", Class: Credential}, at)

	rows := b.Rows(at)
	if len(rows) != 2 {
		t.Fatalf("got %d rows for one reason under two classes, want 2: %+v", len(rows), rows)
	}
	if rows[0].Class == rows[1].Class {
		t.Error("both rows carry the same class, so the reason absorbed the class")
	}
}

// THE BREAKDOWN AGES OUT WITH THE WINDOW.
//
// A view showing a refusal that stopped happening a minute ago describes the
// past, and an operator who learns that stops trusting the surface.
func TestBreakdown_RowsLeaveWhenTheWindowPasses(t *testing.T) {
	at := time.Unix(0, 0).Add(bucketSpan)
	b := NewDenialBreakdown()
	for range 5 {
		b.Add(DenialKey{Reason: "frontdoor/lease-cap-exceeded", Class: Capacity}, at)
	}
	if len(b.Rows(at)) != 1 {
		t.Fatal("the refusals did not register at all")
	}
	if rows := b.Rows(at.Add(Window + bucketSpan)); len(rows) != 0 {
		t.Errorf("a full window later the breakdown still shows %+v; the view would be "+
			"describing something that stopped happening a minute ago", rows)
	}
}

// AND IT STAYS BOUNDED, BECAUSE REASONS ARE NOT AS FINITE AS THEY LOOK.
//
// The declared vocabulary is finite today and this is keyed by a string that
// arrives at runtime. A future producer deriving a reason from anything a peer
// controls turns this into the same memory-exhaustion primitive an unbounded
// source map would be. The cap costs nothing and removes a class of mistake that
// would otherwise rely on everybody remembering.
func TestBreakdown_ItStaysBoundedAndSaysWhatItDropped(t *testing.T) {
	at := time.Unix(0, 0).Add(bucketSpan)
	b := NewDenialBreakdown()
	for i := range 200 {
		b.Add(DenialKey{Reason: fmt.Sprintf("reason-%03d", i), Class: Capacity},
			at.Add(time.Duration(i)*time.Millisecond))
	}
	if n := len(b.Rows(at.Add(200 * time.Millisecond))); n > MaxSubjects {
		t.Errorf("held %d rows after 200 distinct reasons, want at most %d", n, MaxSubjects)
	}
	if b.Omitted() == 0 {
		t.Error("nothing was reported as omitted after 200 reasons arrived; a truncated " +
			"list without its remainder tells the operator the problem is smaller than it is")
	}
}
