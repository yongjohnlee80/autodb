package pressure

import (
	"testing"
	"time"
)

// A SNAPSHOT SAYS WHEN IT WAS TRUE, ON THE TRACKER'S OWN CLOCK.
//
// THE FIGURES OUTLIVE THE MOMENT THEY DESCRIBE. A view renders them, an
// operator reads them minutes later, and nothing on the screen distinguishes a
// pool that is full now from one that was full when the float opened. The stamp
// is what makes that distinguishable, so it is assembled with the figures
// rather than added by whoever happens to render them.
//
// THE TRACKER'S CLOCK, NOT time.Now: this package measures its windows against
// one injected clock precisely so a cell can control them, and a stamp that
// reached for the wall clock instead would be the one figure in the snapshot a
// cell could not drive.
func TestAssemble_TheSnapshotCarriesTheInstantItWasAssembled(t *testing.T) {
	// Deliberately nowhere near the day this runs: a stamp taken from
	// time.Now would land on today, and a cell that asserted "roughly now"
	// would pass against exactly the defect it is here to catch.
	at := time.Date(2001, 9, 9, 1, 46, 40, 0, time.UTC)
	tr := NewTracker(func() time.Time { return at })

	got := tr.Assemble(ViewInput{}).TakenAt
	if !got.Equal(at) {
		t.Errorf("TakenAt = %v, want %v — a snapshot whose stamp comes from "+
			"somewhere other than the tracker's clock cannot be driven by a cell, "+
			"and on a host whose clock has been corrected it does not agree with "+
			"the windows it sits beside", got, at)
	}
}

// AND IT MOVES WITH THE CLOCK, so the stamp is a reading rather than a constant
// captured once.
//
// THE CONTROL FOR THE CELL ABOVE. A tracker that stamped its construction time,
// or any fixed value, satisfies a single-instant assertion perfectly. Two
// assemblies at two instants is what separates "reads the clock" from "happens
// to match".
func TestAssemble_EachAssemblyIsStampedAfresh(t *testing.T) {
	at := time.Date(2001, 9, 9, 1, 46, 40, 0, time.UTC)
	tr := NewTracker(func() time.Time { return at })

	first := tr.Assemble(ViewInput{}).TakenAt
	at = at.Add(37 * time.Second)
	second := tr.Assemble(ViewInput{}).TakenAt

	if second.Sub(first) != 37*time.Second {
		t.Errorf("two assemblies 37s apart were stamped %v apart; a stamp that does "+
			"not move is a timestamp the view will show without it ever ageing",
			second.Sub(first))
	}
}
