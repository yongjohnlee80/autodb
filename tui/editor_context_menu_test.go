package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// screenCell finds text on the screen and answers the CELL it starts at. It
// walks the backend's cells, not the screen string: the frames are drawn with
// multi-byte box runes and a Korean glyph is two cells wide, so neither a byte
// nor a rune offset is a column.
func screenCell(s *decltest.Screen, text string) (x, y int, ok bool) {
	want := []rune(text)
	for row, cells := range s.Backend.Snapshot() {
		var runes []rune
		var cols []int
		for col, c := range cells {
			if c.Continuation() {
				continue
			}
			content := c.Content
			if content == "" {
				content = " "
			}
			for _, r := range content {
				runes = append(runes, r)
				cols = append(cols, col)
			}
		}
		for i := 0; i+len(want) <= len(runes); i++ {
			if string(runes[i:i+len(want)]) == text {
				return cols[i], row, true
			}
		}
	}
	return 0, 0, false
}

// THE QUERY EDITOR'S RIGHT-CLICK MENU acts as the Edit menu does: Paste puts the
// register back into the query, and the open note is marked unsaved. Its rows
// are golib's, named by catalog id, so they follow the App's language.
func TestTheQueryEditorsRightClickMenuPastesAndMarksTheNoteUnsaved(t *testing.T) {
	for _, tc := range []struct{ lang, paste string }{{"en", "Paste"}, {"ko_KR", "붙여넣기"}} {
		t.Run(tc.lang, func(t *testing.T) {
			h, s, _ := notesHost(t)
			if tc.lang != "en" {
				h.RunCommand("options.language." + tc.lang)
				s.WaitFor(t, "the bar in Korean", func(sc string) bool { return strings.Contains(sc, "옵션") })
			}
			newNote(t, s, "draft")
			typeInto(t, s, "select 1")
			// Unsaved, then saved: the language-free signal (the "saved" message
			// is translated).
			s.WaitFor(t, "the typed note unsaved", func(string) bool { return strings.Contains(lastRow(s), "draft.sql [+]") })
			s.Keys(t, key(' '), key('s'))
			s.WaitFor(t, "the note saved", func(string) bool {
				return strings.Contains(lastRow(s), "draft.sql") && !strings.Contains(lastRow(s), "[+]")
			})
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
			s.WaitForText(t, tc.paste)
			px, py, _ := screenCell(s, tc.paste)
			s.Keys(t,
				tuicore.MouseEvent{Kind: tuicore.MousePress, Button: tuicore.MouseLeft, X: px, Y: py},
				tuicore.MouseEvent{Kind: tuicore.MouseRelease, Button: tuicore.MouseLeft, X: px, Y: py})
			s.WaitFor(t, "Paste reached the query and marked the note unsaved", func(string) bool {
				value, _, _ := h.QueryAndRegister()
				return strings.Count(value, "select 1") == 2 && strings.Contains(lastRow(s), "draft.sql [+]")
			})
			if strings.Contains(s.String(), tc.paste) {
				t.Errorf("the menu is still open after choosing Paste:\n%s", s.String())
			}
		})
	}
}
