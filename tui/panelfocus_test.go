package tui

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// THE EXPLORER'S CURSOR FOLLOWS THE KEYBOARD, DRIVEN BY THE REAL KEYS.
//
// Johno, on the droplet: "focus is in the query editor, but cyan highlighter in
// the explorer, when explorer highlighted then the selection becomes gray."
// That is the shape of a value that is one transition BEHIND, not one that is
// inverted — and the difference is only visible if the focus moves the way an
// operator moves it. An earlier probe drove focusPane() directly and passed.
func TestPanels_TheExplorerCursorFollowsTheLeaderKeys(t *testing.T) {
	const liveBG, dimBG = 6, 8
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.explorer.tree.SetRoots(
			widget.NewTreeNode("ws:1", "alpha", widget.WithLeaf()),
			widget.NewTreeNode("ws:2", "beta", widget.WithLeaf()),
		)
	})
	h.settle()

	bg := func() int {
		at, _ := attrsOf(t, h, "alpha")
		if at.BG.Kind != tuicore.CellColorANSI {
			return -1
		}
		return int(at.BG.Index)
	}
	// where reports which pane golib thinks holds the keyboard.
	where := func() string {
		var s string
		h.on(func() {
			switch {
			case h.m.ctx.FocusWithin(h.m.explorerBox):
				s = "explorer"
			case h.m.ctx.FocusWithin(h.m.editorBox):
				s = "editor"
			case h.m.ctx.FocusWithin(h.m.resultsBox):
				s = "results"
			default:
				s = "nowhere"
			}
		})
		return s
	}

	// SPC e — the explorer.
	h.key(' ')
	h.key('e')
	h.settle()
	if got := where(); got != "explorer" {
		t.Fatalf("SPC e put the keyboard on %q, not the explorer", got)
	}
	if got := bg(); got != liveBG {
		t.Errorf("with the keyboard on the EXPLORER its cursor is ANSI %d, want %d (cyan)", got, liveBG)
	}

	// SPC q — the query editor. The explorer must give the accent up.
	h.key(' ')
	h.key('q')
	h.settle()
	if got := where(); got != "editor" {
		t.Fatalf("SPC q put the keyboard on %q, not the editor", got)
	}
	if got := bg(); got != dimBG {
		t.Errorf("with the keyboard on the EDITOR the explorer cursor is ANSI %d, want %d (gray) — "+
			"the accent is on the pane the keyboard LEFT", got, dimBG)
	}

	// And back, to catch a value that merely lags by one.
	h.key(' ')
	h.key('e')
	h.settle()
	if got := bg(); got != liveBG {
		t.Errorf("returning to the explorer, its cursor is ANSI %d, want %d (cyan)", got, liveBG)
	}
}
