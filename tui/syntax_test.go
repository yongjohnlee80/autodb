package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// syntax_test.go holds SQL highlighting to what it promises, on the screen:
// the colour a word is painted in, under the dark theme main.qml imports.

// darkKeyword is dark.qml's syntax.keyword, #87afd7.
var darkKeyword = tuicore.CellColor{Kind: tuicore.CellColorRGB, R: 0x87, G: 0xaf, B: 0xd7}

// fgOfWord is the foreground of word's first cell on the first screen row
// holding marker; ok is false when no row does.
func fgOfWord(s *decltest.Screen, marker, word string) (tuicore.CellColor, bool) {
	return fgOfWordBelow(s, "", marker, word)
}

// rowBelow reports whether a screen row after the first one holding anchor
// holds text: a table's row, not the same text in the pane behind it.
func rowBelow(s *decltest.Screen, anchor, text string) bool {
	below := false
	for _, row := range s.Backend.Snapshot() {
		var line strings.Builder
		for _, c := range row {
			line.WriteString(c.Content)
		}
		if !below {
			below = strings.Contains(line.String(), anchor)
			continue
		}
		if strings.Contains(line.String(), text) {
			return true
		}
	}
	return false
}

// fgOfWordBelow is fgOfWord searching only the rows after the first row that
// holds anchor ("" for every row): a card's text, not the same text in the
// pane behind it.
func fgOfWordBelow(s *decltest.Screen, anchor, marker, word string) (tuicore.CellColor, bool) {
	below := anchor == ""
	for _, row := range s.Backend.Snapshot() {
		var line strings.Builder
		var cols []int
		for x, c := range row {
			line.WriteString(c.Content)
			for range len(c.Content) {
				cols = append(cols, x)
			}
		}
		text := line.String()
		if !below {
			below = strings.Contains(text, anchor)
			continue
		}
		if !strings.Contains(text, marker) {
			continue
		}
		at := strings.Index(text, marker) + strings.Index(marker, word)
		if at < 0 || at >= len(cols) {
			return tuicore.CellColor{}, false
		}
		return row[cols[at]].Attrs.FG, true
	}
	return tuicore.CellColor{}, false
}

func waitWordFG(t *testing.T, s *decltest.Screen, marker, word string, want bool) {
	t.Helper()
	waitWordFGBelow(t, s, "", marker, word, want)
}

func waitWordFGBelow(t *testing.T, s *decltest.Screen, anchor, marker, word string, want bool) {
	t.Helper()
	s.WaitFor(t, word+" coloured as a keyword="+map[bool]string{true: "yes", false: "no"}[want], func(string) bool {
		fg, ok := fgOfWordBelow(s, anchor, marker, word)
		return ok && (fg == darkKeyword) == want
	})
}

// The query is SQL in its connection's dialect. With no connection it reads
// as PostgreSQL, where `order` in `[order]` is the keyword ORDER; on bravo, a
// SQLite connection, [order] is a bracketed NAME and not coloured — which is
// only true if the highlighter followed the connection.
func TestTheQueryIsHighlightedInItsConnectionsSQL(t *testing.T) {
	_, s := signedIn(t)
	s.WaitForText(t, "main")
	typeInto(t, s, "select [order] from items")
	const marker = "select [order] from items"
	waitWordFG(t, s, marker, "select", true)
	waitWordFG(t, s, marker, "order", true) // PostgreSQL: ORDER

	s.Keys(t, key(' '), key('C'))
	s.WaitFor(t, "the picker", func(sc string) bool { return strings.Contains(sc, "bravo  sqlite") })
	s.Keys(t, enter())
	s.WaitForText(t, "query → bravo")
	waitWordFG(t, s, marker, "select", true)
	waitWordFG(t, s, marker, "order", false) // SQLite: a [name]
}

// A history script opens coloured as ITS connection's SQL, found by the
// connection's id: `select [order] from items`, run on bravo (SQLite), shows
// [order] as a name — a PostgreSQL reading, the fallback for an unknown
// connection, would colour ORDER.
func TestAHistoryScriptIsHighlightedInItsConnectionsSQL(t *testing.T) {
	_, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('C'))
	s.WaitFor(t, "the picker", func(sc string) bool { return strings.Contains(sc, "bravo  sqlite") })
	s.Keys(t, enter())
	s.WaitForText(t, "query → bravo")
	typeInto(t, s, "select [order] from items")
	s.Keys(t, key(' '), key('r'))
	// The run's OWN answer — bravo has no column "order", which the wire
	// reports as an internal error with its detail withheld — not the editor's
	// text, which holds "order" before the run is even sent.
	s.WaitFor(t, "the run answered", func(string) bool { return strings.Contains(lastRow(s), "internal error") })

	s.Keys(t, key(' '), key('H'))
	s.WaitFor(t, "the run in the history table", func(sc string) bool {
		return !strings.Contains(sc, "loading…") && rowBelow(s, "┌ history ", "[order]")
	})
	s.Keys(t, enter()) // the newest row: the run above
	s.WaitForText(t, "┌ script — ")
	// In the CARD: the query editor behind it holds the same text.
	waitWordFGBelow(t, s, "┌ script — ", "select [order] from items", "select", true)
	waitWordFGBelow(t, s, "┌ script — ", "select [order] from items", "order", false)
}
