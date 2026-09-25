package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// zoom_test.go holds the three panes: moving between them, and one at a time.

func ctrl(r rune) tuicore.KeyEvent {
	return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: r, Mods: tuicore.ModCtrl}
}

// Ctrl+h/j/k/l move the keyboard between the panes: the explorer left of the
// query, the query above the results.
func TestCtrlHJKLMovesBetweenThePanes(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	for _, step := range []struct {
		key  rune
		want string
	}{{'h', "explorerTree"}, {'l', "editor"}, {'k', "editor"}} {
		s.Keys(t, ctrl(step.key))
		s.WaitFor(t, "focus on "+step.want, func(string) bool { return h.PaneWithFocus() == step.want })
	}
	// Down, with nothing run yet: the results hold nothing to take the
	// keyboard, so it stays on the query. (With rows, see the run test.)
	s.Keys(t, ctrl('j'), ctrl('h'))
	s.WaitFor(t, "still moving from the query", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
}

// SPC z gives the pane in use the screen, and SPC z again gives it back;
// Zoom out is refused while nothing is zoomed.
func TestSpaceZZoomsThePaneInUse(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	h.RunCommand("view.zoom_out") // not offered while nothing is zoomed: nothing happens
	s.Keys(t, ctrl('h'), ctrl('l'))
	s.WaitFor(t, "all panes, the query in use", func(sc string) bool {
		return h.PaneWithFocus() == "editor" && strings.Contains(sc, "┌ explorer") && strings.Contains(sc, "┌ results")
	})

	s.Keys(t, key(' '), key('z')) // the query editor has the keyboard
	s.WaitFor(t, "the query alone", func(sc string) bool {
		return strings.Contains(sc, "┌ query") && !strings.Contains(sc, "┌ explorer") && !strings.Contains(sc, "┌ results")
	})
	s.Keys(t, key(' '), key('z'))
	s.WaitFor(t, "all three back", func(sc string) bool {
		return strings.Contains(sc, "┌ explorer") && strings.Contains(sc, "┌ results")
	})

	s.Keys(t, ctrl('h'), key(' '), key('z')) // the explorer, zoomed
	s.WaitFor(t, "the explorer alone", func(sc string) bool {
		return strings.Contains(sc, "┌ explorer") && !strings.Contains(sc, "┌ results") && !strings.Contains(sc, "┌ query")
	})
	// Moving to a hidden pane leaves the zoom first.
	s.Keys(t, ctrl('l'))
	s.WaitFor(t, "out of the zoom, on the query", func(sc string) bool {
		return h.PaneWithFocus() == "editor" && strings.Contains(sc, "┌ explorer") && strings.Contains(sc, "┌ results")
	})
}
