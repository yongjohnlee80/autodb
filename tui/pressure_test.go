package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
)

func renderedPressure(rows []pressureRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.label)
		b.WriteString("|")
		b.WriteString(r.value)
		b.WriteString("\n")
	}
	return b.String()
}

// A VIEW THAT CANNOT READ SAYS SO, RATHER THAN SHOWING AN EMPTY TABLE.
//
// THIS IS THE FAILURE THE WHOLE SCOPE EXISTS TO AVOID, one layer up. An empty
// pressure table and a front door under no pressure at all look identical, and
// at three in the morning somebody will read the first as the second. Both the
// unreachable case and the unwired case must name themselves.
func TestPressureView_AnUnreadableViewNamesItselfInsteadOfLookingCalm(t *testing.T) {
	var m Model // no source wired
	got := renderedPressure(m.pressureRows())
	if !strings.Contains(got, "unavailable") {
		t.Errorf("an unwired view rendered %q; an empty table is indistinguishable from "+
			"a front door under no pressure", got)
	}

	m.pressure = failingSource{errors.New("daemon not reachable")}
	got = renderedPressure(m.pressureRows())
	if !strings.Contains(got, "unavailable") || !strings.Contains(got, "daemon not reachable") {
		t.Errorf("a failed read rendered %q; it must name what went wrong", got)
	}
}

type failingSource struct{ err error }

func (f failingSource) Pressure() (pressure.Snapshot, error) {
	return pressure.Snapshot{}, f.err
}

// EVERY REFUSAL CARRIES ITS CLASS, BESIDE IT, ALWAYS.
//
// "Somebody is hammering us" and "we have run out of room" are answered in
// opposite directions. A column that merged them, or showed a bare count, would
// have sent the incident's operator to resize nothing or to chase nobody.
func TestPressureView_EveryRefusalShowsWhetherItIsUsOrThem(t *testing.T) {
	got := renderedPressure(pressureSnapshotRows(pressure.Snapshot{
		Denials: []pressure.DenialRow{
			{DenialKey: pressure.DenialKey{Reason: "frontdoor/lease-cap-exceeded",
				Class: pressure.Capacity}, Count: 7},
			{DenialKey: pressure.DenialKey{Reason: "frontdoor/denied",
				Class: pressure.Credential}, Count: 2},
		},
	}))
	if !strings.Contains(got, "capacity") || !strings.Contains(got, "credential") {
		t.Errorf("rendered %q without naming both classes; a bare count sends somebody "+
			"to resize a pool over password guessing, or the reverse", got)
	}
}

// A TRUNCATED SECTION SAYS WHAT IT LEFT OUT, ON THE SCREEN.
//
// The model carries the remainder; this asserts it actually reaches the surface.
// Sixteen rows with no remainder tells somebody the problem is smaller than it is.
func TestPressureView_TruncationIsVisibleToTheOperator(t *testing.T) {
	s := pressure.Snapshot{
		PerUser:          []pressure.Row{{Label: "sessions.user", Subject: "1", Value: 9, Cap: 10}},
		PerUserOmitted:   24,
		Throttled:        []pressure.ThrottledRow{{Host: "10.0.0.1", Remaining: 30 * time.Second}},
		ThrottledOmitted: 11,
	}
	got := renderedPressure(pressureSnapshotRows(s))
	for _, want := range []string{"and 24 more", "and 11 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %q without %q; a list truncated without its remainder "+
				"tells the operator the problem is smaller than it is", got, want)
		}
	}
}

// AN UNCONFIGURED CAP IS NOT RENDERED AS A BREACH.
//
// "3 / 0" invites somebody to read an unlimited dimension as three over its
// limit, and act on it.
func TestPressureView_NoLimitIsNotShownAsZero(t *testing.T) {
	got := renderedPressure(pressureSnapshotRows(pressure.Snapshot{
		Sessions: pressure.Row{Label: "sessions.global", Value: 3, Cap: 0},
	}))
	if strings.Contains(got, "3 / 0") {
		t.Errorf("rendered %q; an unconfigured cap shown as zero reads as a breach", got)
	}
	if !strings.Contains(got, "no limit") {
		t.Errorf("rendered %q without saying the dimension has no limit", got)
	}
}

// A WAIT IS RENDERED IN SOMETHING A PERSON CAN ACT ON.
//
// A throttle expiring in 30000000000ns is a true statement nobody can use.
func TestPressureView_AWaitIsReadable(t *testing.T) {
	rows := pressureSnapshotRows(pressure.Snapshot{
		Throttled: []pressure.ThrottledRow{{Host: "10.0.0.1", Remaining: 30*time.Second + 400*time.Millisecond}},
	})
	// ASSERTED ON THE ROW, NOT THE WHOLE SCREEN. The first version searched the
	// rendered blob for "ns" and tripped on the word "connections" — a cell
	// failing on its own carelessness rather than on the code, which wastes
	// exactly the attention a red test is supposed to buy.
	var value string
	for _, r := range rows {
		if strings.Contains(r.label, "10.0.0.1") {
			value = r.value
		}
	}
	if value == "" {
		t.Fatal("the throttled source was not rendered at all")
	}
	if !strings.Contains(value, "30s") {
		t.Errorf("rendered %q; a wait an operator cannot read is one they cannot act on", value)
	}
	if strings.Contains(value, "ns") || strings.Contains(value, "400ms") {
		t.Errorf("rendered %q with sub-second precision nobody needs", value)
	}
}
