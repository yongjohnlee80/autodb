package tui

// WHAT THE OPERATOR ACTUALLY SEES.
//
// The axis reached the client and then had nowhere to appear: the history table
// rendered "100" for a page that was cut off by the row limit and "100" for a
// statement that returned everything it had. Two different facts, one display.
//
// The mark goes on the ROW COUNT, not on the status column, and that placement
// is the design rather than a convenience. Status is the durability token --
// "ok" means the effects committed, which is equally true of a suspended
// Execute -- so folding suspension into it would trade one unanswerable
// question for another. Suspension answers "were there more rows?", which is a
// statement about the count.

import (
	"strings"
	"testing"
)

func TestRowCountText_MarksASuspendedPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		row  HistoryRow
		want string
	}{
		{
			name: "suspended: there were more rows",
			row:  HistoryRow{RowCount: 100, Suspended: true},
			want: "100+",
		},
		{
			// The control. Without it a display that always appended "+" would
			// satisfy the case above and distinguish nothing.
			name: "completed: that was all of them",
			row:  HistoryRow{RowCount: 100, Suspended: false},
			want: "100",
		},
		{
			// Zero rows and suspended is a real combination: a limit of zero
			// rows delivered still leaves the statement unfinished.
			name: "suspended with nothing delivered",
			row:  HistoryRow{RowCount: 0, Suspended: true},
			want: "0+",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rowCountText(tc.row); got != tc.want {
				t.Errorf("rowCountText = %q, want %q", got, tc.want)
			}
		})
	}
}

// AND THE DETAIL VIEW SPELLS IT OUT, because a "+" in a six-column table is a
// hint rather than an explanation. This is where an operator finds out what it
// meant.
func TestScriptTitle_NamesSuspensionInWords(t *testing.T) {
	t.Parallel()

	suspended := scriptTitle(HistoryRow{
		StartedAt: "2026-09-09T12:00:00Z", User: "root", Status: "ok", Suspended: true,
	})
	if !strings.Contains(suspended, "suspended") {
		t.Errorf("the detail title never says suspended: %q", suspended)
	}
	// THE DURABILITY TOKEN IS UNCHANGED AND STILL SHOWN. The title carries
	// both facts, because "did it commit?" and "did it finish?" are different
	// questions and an operator needs both answered.
	if !strings.Contains(suspended, "ok") {
		t.Errorf("the detail title dropped the status: %q", suspended)
	}

	// The control: an unsuspended row says nothing about suspension. A title
	// that mentioned it unconditionally would pass the assertion above.
	completed := scriptTitle(HistoryRow{
		StartedAt: "2026-09-09T12:00:00Z", User: "root", Status: "ok", Suspended: false,
	})
	if strings.Contains(completed, "suspended") {
		t.Errorf("a completed statement's title claims suspension: %q", completed)
	}
}
