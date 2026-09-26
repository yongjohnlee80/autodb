package tui_test

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// Edit menu commands reach the query Editor even when the menu or another pane
// has focus. Copy does not dirty a note; Cut and Paste do, using its register.
func TestEditMenuUsesTheEditorRegisterAndTracksUnsavedNotes(t *testing.T) {
	for _, keyset := range []string{"vim", "textedit"} {
		t.Run(keyset, func(t *testing.T) {
			h, s, _ := notesHost(t)
			newNote(t, s, "draft")
			typeInto(t, s, "select 1")
			s.Keys(t, key(' '), key('s'))
			s.WaitForText(t, "saved draft.sql")
			if keyset == "textedit" {
				h.RunCommand("options.editor.textedit")
				s.WaitFor(t, "TextEdit selected", func(string) bool { return h.Keyset() == keyset })
			}
			s.WaitFor(t, "Edit menu available", func(sc string) bool {
				return strings.Contains(strings.Split(sc, "\n")[0], "Edit")
			})
			s.Keys(t, ctrl('h')) // the editor no longer owns focus
			s.WaitFor(t, "explorer focused", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
			s.Keys(t, decltest.Alt('e'))
			s.WaitForText(t, "Copy/Yank (register; clipboard if available)")
			s.Keys(t, key('c')) // actual QML Edit menu projection, not a synthetic Editor key
			s.WaitFor(t, "Copy reached the query", func(string) bool {
				_, reg, linewise := h.QueryAndRegister()
				return reg == "select 1" && linewise && !strings.Contains(lastRow(s), "[+]")
			})
			h.RunCommand("edit.cut")
			s.WaitFor(t, "Cut marked the note unsaved", func(string) bool {
				value, reg, linewise := h.QueryAndRegister()
				return value == "" && reg == "select 1" && linewise && strings.Contains(lastRow(s), "draft.sql [+]")
			})
			h.RunCommand("edit.paste")
			s.WaitFor(t, "Paste restored the register without marking the note saved", func(string) bool {
				value, _, _ := h.QueryAndRegister()
				return strings.HasPrefix(value, "select 1") && strings.Contains(lastRow(s), "draft.sql [+]")
			})
		})
	}
}
