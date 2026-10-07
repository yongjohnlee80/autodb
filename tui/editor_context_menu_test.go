package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// screenCell finds text on the screen and answers its column in CELLS, not
// bytes: the frames around it are drawn with multi-byte box runes.
func screenCell(s *decltest.Screen, text string) (x, y int, ok bool) {
	for row, line := range strings.Split(s.String(), "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return len([]rune(line[:i])), row, true
		}
	}
	return 0, 0, false
}

// THE QUERY EDITOR'S RIGHT-CLICK MENU acts as the Edit menu does: Paste puts the
// register back into the query, and the open note is marked unsaved.
func TestTheQueryEditorsRightClickMenuPastesAndMarksTheNoteUnsaved(t *testing.T) {
	h, s, _ := notesHost(t)
	newNote(t, s, "draft")
	typeInto(t, s, "select 1")
	s.Keys(t, key(' '), key('s'))
	s.WaitForText(t, "saved draft.sql")
	h.RunCommand("edit.copy") // the line, linewise, into the register
	s.WaitFor(t, "the register holds the line", func(string) bool {
		_, reg, _ := h.QueryAndRegister()
		return reg == "select 1"
	})

	x, y, ok := screenCell(s, "select 1")
	if !ok {
		t.Fatalf("the query is not on screen:\n%s", s.String())
	}
	s.Keys(t, tuicore.MouseEvent{Kind: tuicore.MousePress, Button: tuicore.MouseRight, X: x + 2, Y: y})
	s.WaitForText(t, "Paste")
	px, py, _ := screenCell(s, "Paste")
	s.Keys(t,
		tuicore.MouseEvent{Kind: tuicore.MousePress, Button: tuicore.MouseLeft, X: px, Y: py},
		tuicore.MouseEvent{Kind: tuicore.MouseRelease, Button: tuicore.MouseLeft, X: px, Y: py})
	s.WaitFor(t, "Paste reached the query and marked the note unsaved", func(string) bool {
		value, _, _ := h.QueryAndRegister()
		return strings.Count(value, "select 1") == 2 && strings.Contains(lastRow(s), "draft.sql [+]")
	})
	if strings.Contains(s.String(), "Paste") {
		t.Errorf("the menu is still open after choosing Paste:\n%s", s.String())
	}
}
