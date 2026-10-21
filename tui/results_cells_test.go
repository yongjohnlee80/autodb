package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// The results are a table with a cursor on cells that sorts by what each cell holds: s on the
// numbers' column orders them 9, 10, 100 with the NULL last (as text they would be 10, 100, 9), and
// Enter on the first row shown opens the result's own row for it, however it is sorted.
func TestTheResultsSortByWhatTheirCellsHold(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	h.ShowResult([]string{"name", "qty"}, [][]any{{"alpha", int64(10)}, {"bravo", int64(9)}, {"charlie", nil}, {"delta", int64(100)}})
	s.WaitFor(t, "the rows", func(sc string) bool { return strings.Contains(sc, "alpha") && strings.Contains(sc, "delta") })
	s.Keys(t, ctrl('j'))
	s.WaitFor(t, "the results in use", func(string) bool { return h.PaneWithFocus() == "results" })
	s.Keys(t, key('l'), key('s')) // the cursor to qty, then sort by it
	s.WaitFor(t, "sorted by qty: 9, 10, 100, then NULL", func(string) bool {
		b, a, d, c := rowOf(s, "bravo"), rowOf(s, "alpha"), rowOf(s, "delta"), rowOf(s, "charlie")
		return b >= 0 && b < a && a < d && d < c
	})
	s.Keys(t, key('g'), key('g'), enter()) // the first row shown: bravo, the result's second
	s.WaitFor(t, "bravo's row opened", func(sc string) bool {
		return strings.Contains(sc, "┌ row ") && strings.Contains(sc, "name = bravo") && strings.Contains(sc, "qty = 9")
	})
}

// What the results' table yanks is the query editor's to paste: y on a cell puts it in the
// editor's register as well as on the clipboard, and p in the query pastes it.
func TestAResultYankPastesInTheQuery(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	h.ShowResult([]string{"name", "qty"}, [][]any{{"alpha", int64(10)}, {"bravo", int64(9)}})
	s.WaitFor(t, "the rows", func(sc string) bool { return strings.Contains(sc, "bravo") })
	s.Keys(t, ctrl('j'))
	s.WaitFor(t, "the results in use", func(string) bool { return h.PaneWithFocus() == "results" })
	s.Keys(t, key('j'), key('y')) // bravo's name
	s.WaitFor(t, "the yank in the editor's register", func(string) bool {
		_, reg, _ := h.QueryAndRegister()
		return reg == "bravo" && string(s.Backend.Clipboard()) == "bravo"
	})
	s.Keys(t, ctrl('k'), esc(), key('p'))
	s.WaitFor(t, "pasted in the query", func(string) bool { q, _, _ := h.QueryAndRegister(); return q == "bravo" })
}

// The results number their rows, and a press on a row's number selects the row: y copies it whole.
func TestAResultRowsNumberSelectsTheRow(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	h.ShowResult([]string{"name", "qty"}, [][]any{{"alpha", int64(10)}, {"bravo", int64(9)}})
	s.WaitFor(t, "the rows numbered", func(string) bool {
		y := rowOf(s, "bravo")
		return y >= 0 && strings.Contains(rows(s)[y], "2 bravo")
	})
	y := rowOf(s, "bravo")
	x := strings.Index(rows(s)[y], "2 bravo")
	x = len([]rune(rows(s)[y][:x]))
	s.Keys(t, tuicore.MouseEvent{Kind: tuicore.MousePress, Button: tuicore.MouseLeft, X: x, Y: y},
		tuicore.MouseEvent{Kind: tuicore.MouseRelease, Button: tuicore.MouseLeft, X: x, Y: y}, key('y'))
	s.WaitFor(t, "bravo's row copied", func(string) bool { return string(s.Backend.Clipboard()) == "bravo\t9" })
}
