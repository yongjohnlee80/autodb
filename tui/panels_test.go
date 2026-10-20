package tui_test

import (
	"slices"
	"strings"
	"testing"

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

// The screen opens as the split it was: the explorer docked on the left below the menu bar, 25% of
// 120 cells, the query beside it, and the results under the query, 40% of 32 rows, beside the
// explorer. The panels take their strips from the page, so the query starts where the explorer ends.
func TestThePanelsOpenDockedBesideTheQuery(t *testing.T) {
	_, s := signedIn(t)
	s.WaitFor(t, "the docked panels", func(sc string) bool {
		ex, ey, eok := frameAt(sc, "explorer")
		qx, qy, qok := frameAt(sc, "query")
		rx, ry, rok := frameAt(sc, "results")
		return eok && ex == 0 && ey == 1 && qok && qx == 30 && qy == 1 && rok && rx == 30 && ry == 18
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

// A panel's geometry is the account's: the explorer resized and the results floated are stored, and
// a fresh program signed in as the same account wears them.
func TestAPanelsGeometryIsTheAccounts(t *testing.T) {
	addr := seeded(t)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	// the sign-in's read of each of the six geometry preferences, then three writes: the explorer's
	// size and length, and the results' place
	s.WaitFor(t, "the account's geometry read", func(string) bool { return h.PanelPrefsSettled() >= 6 })
	h.PanelResized("explorer", 40, 100)
	h.PanelPlaced("results", [4]float64{10, 20, 50, 40})
	s.WaitFor(t, "the three writes back", func(string) bool { return h.PanelPrefsSettled() >= 9 })
	if strings.Contains(lastRow(s), "saving it failed") {
		t.Fatalf("a geometry write failed: %q", lastRow(s))
	}

	h2, s2 := runHostSized(t, addr, 120, 32)
	loginAs(t, s2, "root", rootPass)
	s2.WaitFor(t, "root's geometry back", func(string) bool {
		return h2.Auth() == "signed-in" && h2.SourceText("App.explorerSize") == "40" && h2.SourceBool("App.resultsFloating")
	})
	if got := h2.SourceText("App.resultsX"); got != "10" {
		t.Fatalf("the results float at x %s; want the stored 10", got)
	}
}

// The dock lists the query and the open panels. A shown panel's icon hides it and gives the
// keyboard to the query, and its next click shows it again; the query's icon hides every shown
// panel, and its next click shows them.
func TestTheDockHidesAndShowsThePanels(t *testing.T) {
	h, s := signedIn(t)
	s.WaitFor(t, "the dock", func(string) bool {
		return slices.Equal(h.DockKeys(), []string{"editor", "explorer", "results"})
	})
	h.GoDock("explorer")
	s.WaitFor(t, "the explorer hidden", func(sc string) bool {
		_, _, ok := frameAt(sc, "explorer")
		return !ok && !h.PanelShown("explorer") && h.PaneWithFocus() == "editor"
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
		return !e && !r && q && qx == 0 // the query has the Window
	})
	h.GoDock("editor")
	s.WaitFor(t, "both back", func(sc string) bool {
		return h.PanelShown("explorer") && h.PanelShown("results")
	})
}

// A dialog over a floating panel still has the keyboard: Ctrl+h, which would move to the explorer,
// is the dialog's while it is open.
func TestADialogOverAFloatingPanelHasTheKeyboard(t *testing.T) {
	h, s := signedIn(t)
	h.PanelPlaced("explorer", [4]float64{5, 10, 30, 50})
	s.WaitFor(t, "the explorer floating", func(string) bool { return h.SourceBool("App.explorerFloating") })
	h.RunCommand("app.about")
	s.WaitFor(t, "about open", func(sc string) bool { return strings.Contains(sc, "┌ About autodb") })
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "about still open", func(sc string) bool { return strings.Contains(sc, "┌ About autodb") })
	if got := h.PaneWithFocus(); got == "explorerTree" {
		t.Fatal("Ctrl+h reached the floating explorer under an open dialog")
	}
}
