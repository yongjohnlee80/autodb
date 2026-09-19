package tui

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// THE CURSOR ROW CARRIES THE COLOURS IT WAS GIVEN AND NO ATTRIBUTE IT WAS NOT.
//
// A terminal applies the reverse attribute by SWAPPING foreground and
// background; the test backend records it as a bit beside colours it leaves
// alone. So a cell that asserts BG.Index alone is green over a row a terminal
// paints inside out -- which is what every cursor cell in this package did
// while the production accent sat on the wrong pane for two days. This one
// reads the bit. Both states are checked, because the inherited reverse rode
// through every SetStyles and flipped both of them.
func TestPanels_TheCursorRowCarriesNoInheritedAttributes(t *testing.T) {
	const liveBG, dimBG = 6, 8
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.explorer.tree.SetRoots(
			widget.NewTreeNode("ws:1", "alpha", widget.WithLeaf()),
			widget.NewTreeNode("ws:2", "beta", widget.WithLeaf()),
		)
	})
	h.settle()

	check := func(state string, wantBG int) {
		t.Helper()
		at, _ := attrsOf(t, h, "alpha")
		if at.BG.Kind != tuicore.CellColorANSI || int(at.BG.Index) != wantBG {
			t.Errorf("%s: the explorer cursor background is %v/%d, want ANSI %d", state, at.BG.Kind, at.BG.Index, wantBG)
		}
		if at.Mask&tuicore.AttrReverse != 0 {
			t.Errorf("%s: the explorer cursor row carries REVERSE; a terminal will swap its colours and paint ANSI %d as the text, not the bar",
				state, wantBG)
		}
		if at.Mask&tuicore.AttrBold != 0 {
			t.Errorf("%s: the explorer cursor row carries BOLD, which listStyles never asked for", state)
		}
	}

	h.on(func() { h.m.focusPane(h.m.explorer) })
	h.settle()
	check("explorer focused", liveBG)

	h.on(func() { h.m.focusPane(h.m.editor) })
	h.settle()
	check("editor focused", dimBG)
}

// THE SAME BIT, ON A List RATHER THAN A Tree. List's defaults carry Reverse on
// the cursor AND Bold on the selected row, and the row under the cursor in the
// workspace panel is also the selected row, so this surface inherits both.
func TestWorkspacePanel_TheCursorRowsCarryNoInheritedAttributes(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	p := openWSPanel(t, h, []WorkspaceInfo{
		{ID: 1, Name: "alpha", Connections: []ConnInfo{{ID: 10, Name: "a-db", Engine: "sqlite"}}},
	})

	check := func(state, text string) {
		t.Helper()
		at, _ := attrsOf(t, h, text)
		if at.BG.Kind != tuicore.CellColorANSI {
			t.Fatalf("%s: %q has no ANSI background; the cursor style is not being drawn", state, text)
		}
		if at.Mask&tuicore.AttrReverse != 0 {
			t.Errorf("%s: %q carries REVERSE; a terminal will paint it inside out", state, text)
		}
		if at.Mask&tuicore.AttrBold != 0 {
			t.Errorf("%s: %q carries BOLD, which listStyles never asked for", state, text)
		}
	}

	check("workspaces focused", "alpha")
	check("workspaces focused", "a-db")

	h.on(func() { p.focusOn(wsConnections) })
	h.settle()
	check("connections focused", "alpha")
	check("connections focused", "a-db")
}
