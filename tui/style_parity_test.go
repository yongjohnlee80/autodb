package tui_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

func color(r, g, b uint8) tuicore.CellColor {
	return tuicore.CellColor{Kind: tuicore.CellColorRGB, R: r, G: g, B: b}
}

// paintedLabel reads a real terminal cell rather than only the theme source.
// Box borders occupy one column even though their UTF-8 is three bytes.
func paintedLabel(t *testing.T, s *decltest.Screen, label string) tuicore.CellAttrs {
	t.Helper()
	for y, row := range strings.Split(s.String(), "\n") {
		if at := strings.Index(row, label); at >= 0 {
			x := utf8.RuneCountInString(row[:at])
			grid := s.Backend.Snapshot()
			if y < len(grid) && x < len(grid[y]) {
				return grid[y][x].Attrs
			}
		}
	}
	t.Fatalf("%q is not painted on the screen:\n%s", label, s.String())
	return tuicore.CellAttrs{}
}

func TestTheExplorerSelectionChangesLookOnFocusAndReturnsToItsBlurredLook(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "▸ main (1)")
	blurred := paintedLabel(t, s, "main (1)")
	blurredCorner := s.Backend.Snapshot()[1][0].Attrs
	if blurred.FG != color(0x55, 0xff, 0xff) || blurred.BG != color(0x55, 0x55, 0x55) || blurred.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("blurred explorer cursor did not use the inactive palette: %+v", blurred)
	}
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "explorer has focus", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	s.WaitFor(t, "selected row looks focused", func(string) bool {
		return paintedLabel(t, s, "main (1)") != blurred
	})
	focused := paintedLabel(t, s, "main (1)")
	if focused.FG != color(0, 0, 0) || focused.BG != color(0xff, 0xff, 0xff) || focused.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("focused explorer cursor has the wrong color/Reverse mask: %+v", focused)
	}
	if corner := s.Backend.Snapshot()[1][0].Attrs; corner.FG != color(0xff, 0xff, 0xff) || corner.BG != blurredCorner.BG || corner.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("focused frame did not brighten independently of the row: %+v", corner)
	}
	s.Keys(t, enter())
	s.WaitForText(t, "connections")
	if normal := paintedLabel(t, s, "connections"); normal.FG != color(0xff, 0xff, 0x55) || normal.BG != color(0, 0, 0xaa) || normal.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("unselected row ignored the document base/text palette: %+v", normal)
	}
	s.Keys(t, ctrl('l'))
	s.WaitFor(t, "query editor has focus", func(string) bool { return h.PaneWithFocus() == "editor" })
	s.WaitFor(t, "explorer row looks blurred again", func(string) bool {
		return paintedLabel(t, s, "main (1)") == blurred
	})
	if corner := s.Backend.Snapshot()[1][0].Attrs; corner.FG != blurredCorner.FG {
		t.Fatalf("blurred frame did not restore its original border: %+v", corner)
	}
}

func TestTheDisabledZoomOutRowIsDimmedAndCannotRun(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, decltest.Alt('v'), key('z'))
	s.WaitForText(t, "Zoom out")
	disabled := paintedLabel(t, s, "Zoom out")
	enabled := paintedLabel(t, s, "Query editor")
	if disabled == enabled || disabled.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("disabled Zoom out looks active/selected: disabled %+v, enabled %+v", disabled, enabled)
	}
	s.Keys(t, key('o')) // its mnemonic exists but cannot activate a disabled command
	s.WaitFor(t, "disabled command stayed in its menu", func(sc string) bool {
		return strings.Contains(sc, "Zoom out") && h.PaneWithFocus() != "editor"
	})
}

func TestAllShippedThemesKeepTheFocusedExplorerSelectionLegible(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "explorer focus", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	for _, theme := range []string{"retro", "mono", "light", "dark"} {
		h.RunCommand("options.theme." + theme)
		s.WaitFor(t, theme+" explorer cursor", func(sc string) bool {
			if h.Theme() != theme || h.PaneWithFocus() != "explorerTree" || !strings.Contains(sc, "main (1)") {
				return false
			}
			cell := paintedLabel(t, s, "main (1)")
			return cell.FG != cell.BG && cell.Mask&tuicore.AttrReverse == 0
		})
	}
}
