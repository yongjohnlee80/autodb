package tui_test

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/parse/qml"

	"github.com/yongjohnlee80/autodb/core/auth"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// frameAt is where the frame titled title starts on the screen: its "┌" corner.
func frameAt(screen, title string) (x, y int, ok bool) {
	for y, line := range strings.Split(screen, "\n") {
		if i := strings.Index(line, "┌ "+title); i >= 0 {
			return len([]rune(line[:i])), y, true
		}
	}
	return 0, 0, false
}

// The screen opens tiled: the explorer down the left below the menu bar, 25% of 120 cells, the
// query beside it, and the results under the query, 40% of the 30 rows between the bars. The three
// share the page: the query starts where the explorer ends.
func TestThePanelsOpenTiledBesideTheQuery(t *testing.T) {
	_, s := signedIn(t)
	s.WaitFor(t, "the tiles", func(sc string) bool {
		ex, ey, eok := frameAt(sc, "explorer")
		qx, qy, qok := frameAt(sc, "query")
		rx, ry, rok := frameAt(sc, "results")
		return eok && ex == 0 && ey == 1 && qok && qx == 30 && qy == 1 && rok && rx == 30 && ry == 19
	})
}

// The screen's panels are auth.PanelSpecs', the table the server checks their geometry against: a
// panel on one and not the other could be drawn and never stored, or stored and never drawn.
func TestTheScreensPanelsAreThePanelSpecs(t *testing.T) {
	var specs []string
	for _, s := range auth.PanelSpecs {
		specs = append(specs, s.ID)
	}
	if got := tuiapp.PanelIDs(); !slices.Equal(got, specs) {
		t.Fatalf("the screen has panels %v; auth.PanelSpecs has %v", got, specs)
	}
}

// A panel's geometry is the account's: the explorer resized and the results re-docked at the top
// are stored, and a fresh program signed in as the same account wears them; the explorer's size is
// its size at the left, and the results' at the top is their own default until one is stored.
func TestAPanelsGeometryIsTheAccounts(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	// the sign-in's read of each of the six geometry preferences (each panel's edge and its size at
	// its two edges), then two writes: the explorer's size and the results' edge
	s.WaitFor(t, "the account's geometry read", func(string) bool { return h.PanelPrefsSettled() >= 6 })
	h.PanelResized("explorer", 40.25)
	h.PanelDocked("results", "top")
	s.WaitFor(t, "the two writes back", func(string) bool { return h.PanelPrefsSettled() >= 8 })
	if strings.Contains(lastRow(s), "saving it failed") {
		t.Fatalf("a geometry write failed: %q", lastRow(s))
	}

	h2, s2 := runHostSized(t, addr, 120, 32)
	loginAs(t, s2, "root", rootPass)
	s2.WaitFor(t, "root's geometry back", func(string) bool {
		return h2.Auth() == "signed-in" && h2.SourceText("App.explorerSize") == "40.25" && h2.SourceText("App.resultsEdge") == "top"
	})
	s2.WaitFor(t, "the results at the top", func(sc string) bool {
		_, ry, rok := frameAt(sc, "results")
		_, qy, qok := frameAt(sc, "query")
		return rok && ry == 1 && qok && qy > 1
	})
}

// A size keeps two decimals end to end: 40 one-cell steps on a 400-cell-wide explorer reach the
// server and come back as 35, the border 40 cells over; a fresh program recalls 101 cells for a
// stored 25.25.
func TestAPanelsFractionalSizeRoundTrips(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 400, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" && h.PanelPrefsSettled() >= 6 })
	s.WaitFor(t, "the explorer at 100 cells", func(string) bool { _, _, w, _ := h.TileBounds("explorerPanel"); return w == 100 })
	h.TileSteps("explorerPanel", 1, 40)
	s.WaitFor(t, "40 steps stored", func(string) bool {
		_, _, w, _ := h.TileBounds("explorerPanel")
		return w == 140 && h.SourceText("App.explorerSize") == "35" && h.PanelPrefsSettled() >= 7
	})
	h.TileSteps("explorerPanel", -1, 39)
	s.WaitFor(t, "101 cells: 25.25", func(string) bool {
		_, _, w, _ := h.TileBounds("explorerPanel")
		return w == 101 && h.SourceText("App.explorerSize") == "25.25"
	})
	s.WaitFor(t, "every write back", func(string) bool { return h.PanelPrefsIdle() })

	h2, s2 := runHostSized(t, addr, 400, 32)
	loginAs(t, s2, "root", rootPass)
	s2.WaitFor(t, "101 cells recalled", func(string) bool {
		_, _, w, _ := h2.TileBounds("explorerPanel")
		return h2.Auth() == "signed-in" && w == 101
	})
}

// The dock lists the query and the panels. A shown panel's icon hides it and gives the keyboard to
// the query, and its next click shows it again; the query's icon hides every shown panel, and its
// next click shows them.
func TestTheDockHidesAndShowsThePanels(t *testing.T) {
	h, s := signedIn(t)
	s.WaitFor(t, "the dock", func(string) bool {
		return slices.Equal(h.DockKeys(), []string{"editor", "explorer", "results"})
	})
	h.GoDock("explorer")
	s.WaitFor(t, "the explorer hidden", func(sc string) bool {
		_, _, ok := frameAt(sc, "explorer")
		qx, _, q := frameAt(sc, "query")
		return !ok && q && qx == 0 && !h.PanelShown("explorer") && h.PaneWithFocus() == "editor"
	})
	h.GoDock("explorer")
	s.WaitFor(t, "the explorer back", func(sc string) bool {
		_, _, ok := frameAt(sc, "explorer")
		return ok && h.PanelShown("explorer")
	})
	h.GoDock("editor")
	s.WaitFor(t, "both hidden", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		_, _, r := frameAt(sc, "results")
		qx, _, q := frameAt(sc, "query")
		return !e && !r && q && qx == 0 // the query has the page
	})
	h.GoDock("editor")
	s.WaitFor(t, "both back", func(sc string) bool {
		return h.PanelShown("explorer") && h.PanelShown("results")
	})
}

// SPC e toggles the explorer: shown, it is hidden and the query has the keyboard and its space;
// hidden, it is shown and has the keyboard.
func TestSpaceEShowsAndHidesTheExplorer(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, esc(), key(' '), key('e'))
	s.WaitFor(t, "the explorer hidden", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		qx, _, q := frameAt(sc, "query")
		return !e && q && qx == 0 && h.PaneWithFocus() == "editor"
	})
	s.Keys(t, key(' '), key('e'))
	s.WaitFor(t, "the explorer back, in use", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		return e && h.PaneWithFocus() == "explorerTree"
	})
}

// Escape in the explorer hides it, as its dock icon does.
func TestEscapeInAPanelHidesIt(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "the explorer in use", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	s.Keys(t, esc())
	s.WaitFor(t, "the explorer hidden", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		return !e && !h.PanelShown("explorer") && h.PaneWithFocus() == "editor"
	})
}

// A dialog over the tiles has the keyboard: Ctrl+h, which would move to the explorer, is the
// dialog's while it is open.
func TestADialogOverTheTilesHasTheKeyboard(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	h.RunCommand("app.about")
	s.WaitFor(t, "about open", func(sc string) bool { return strings.Contains(sc, "┌ About autodb") })
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "about still open", func(sc string) bool { return strings.Contains(sc, "┌ About autodb") })
	if got := h.PaneWithFocus(); got == "explorerTree" {
		t.Fatal("Ctrl+h reached the explorer under an open dialog")
	}
}

// Hiding the zoomed panel ends its zoom: the next SPC z zooms the query rather than undoing the
// hidden panel's zoom; and SPC e brings the explorer back in its place, not maximized.
func TestHidingAZoomedPanelEndsItsZoom(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("view.zoom_explorer")
	s.WaitFor(t, "the explorer zoomed", func(sc string) bool {
		_, _, q := frameAt(sc, "query")
		_, _, e := frameAt(sc, "explorer")
		return e && !q
	})
	h.GoDock("explorer")
	s.WaitFor(t, "the explorer hidden, the query in use", func(sc string) bool {
		_, _, r := frameAt(sc, "results")
		return !h.PanelShown("explorer") && h.PaneWithFocus() == "editor" && r
	})
	s.Keys(t, key(' '), key('z'))
	s.WaitFor(t, "the query zoomed", func(sc string) bool {
		_, _, q := frameAt(sc, "query")
		_, _, r := frameAt(sc, "results")
		return q && !r
	})
	s.Keys(t, key(' '), key('z')) // out
	s.Keys(t, key(' '), key('e'))
	s.WaitFor(t, "the explorer back in its place", func(sc string) bool {
		ex, ey, e := frameAt(sc, "explorer")
		qx, _, q := frameAt(sc, "query")
		return e && ex == 0 && ey == 1 && q && qx == 30
	})
}

// A panel's dock icon, clicked while the query is zoomed, ends the zoom and gives that panel the
// keyboard; SPC z from it then zooms it.
func TestAPanelsDockIconEndsTheQuerysZoom(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("view.zoom_editor")
	s.WaitFor(t, "the query zoomed", func(sc string) bool { _, _, e := frameAt(sc, "explorer"); return !e })
	h.GoDock("explorer")
	s.WaitFor(t, "the explorer shown, in use", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		_, _, r := frameAt(sc, "results")
		return e && r && h.PaneWithFocus() == "explorerTree"
	})
	s.Keys(t, key(' '), key('z'))
	s.WaitFor(t, "the explorer zoomed", func(sc string) bool {
		_, _, q := frameAt(sc, "query")
		_, _, e := frameAt(sc, "explorer")
		return e && !q
	})
}

// A zoomed panel restored by its own maximize control ends its zoom: SPC z from the query then
// zooms the query, where a zoom still held would only have been undone.
func TestAZoomedPanelRestoredByItsOwnControlEndsTheZoom(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("view.zoom_results")
	s.WaitFor(t, "the results zoomed", func(sc string) bool { _, _, q := frameAt(sc, "query"); return !q })
	h.ToggleMaximize("results")
	s.WaitFor(t, "restored by its own control", func(sc string) bool { _, _, q := frameAt(sc, "query"); return q })
	h.RunCommand("focus.editor")
	s.WaitFor(t, "the query in use", func(string) bool { return h.PaneWithFocus() == "editor" })
	s.Keys(t, key(' '), key('z'))
	s.WaitFor(t, "the query zoomed", func(sc string) bool {
		_, _, e := frameAt(sc, "explorer")
		_, _, r := frameAt(sc, "results")
		return !e && !r
	})
}

// Every TilePanel in main.qml takes its geometry from its spec in auth.PanelSpecs: its edge, size
// and minimum are bound to the panel's sources, its edges are the spec's, and the edge source holds
// one of them. A TilePanel writing a number of its own would be a second policy.
func TestEveryPanelBindsItsSpec(t *testing.T) {
	src, err := os.ReadFile("qml/main.qml")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := (qml.QML{File: "main.qml"}).Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	panels := map[string]*qml.SpecNode{}
	var walk func(n *qml.SpecNode)
	walk = func(n *qml.SpecNode) {
		if n.Type == "TilePanel" {
			panels[n.ID] = n
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tree.Root)
	h, _ := signedIn(t)
	for _, spec := range auth.PanelSpecs {
		node := panels[tuiapp.PanelQMLID(spec.ID)]
		if node == nil {
			t.Errorf("main.qml declares no TilePanel %s for the spec %q", tuiapp.PanelQMLID(spec.ID), spec.ID)
			continue
		}
		bound := map[string]string{}
		for _, p := range node.Props {
			bound[p.Name] = p.Value.Raw
		}
		for prop, source := range map[string]string{"edge": "Edge", "size": "Size", "minimumSize": "MinSize"} {
			if want := "App." + spec.ID + source; bound[prop] != want {
				t.Errorf("%s's %s is %q; want it bound to %s", node.ID, prop, bound[prop], want)
			}
		}
		if want := strings.Join(spec.Edges, " "); bound["edges"] != want {
			t.Errorf("%s's edges are %q; the spec's are %q", node.ID, bound["edges"], want)
		}
		if got := h.SourceText("App." + spec.ID + "Edge"); !slices.Contains(spec.Edges, got) {
			t.Errorf("%s's edge is %q; the spec allows %v", spec.ID, got, spec.Edges)
		}
		if got := h.SourceText("App." + spec.ID + "Size"); got != strconv.Itoa(int(defaultSize(spec.ID))) {
			t.Errorf("%s's size is %s before the account sets one; want its default %v", spec.ID, got, defaultSize(spec.ID))
		}
	}
}

func defaultSize(id string) float64 { return map[string]float64{"explorer": 25, "results": 40}[id] }
