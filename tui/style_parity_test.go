package tui_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
	"github.com/yongjohnlee80/golib/tui/decl/themes"
	"github.com/yongjohnlee80/golib/tui/style"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func color(r, g, b uint8) tuicore.CellColor {
	return tuicore.CellColor{Kind: tuicore.CellColorRGB, R: r, G: g, B: b}
}

// cellColorOf is a theme's colour value as a cell wears it: the terminal's default, an ANSI index,
// or an RGB.
func cellColorOf(t *testing.T, v string) tuicore.CellColor {
	t.Helper()
	c, err := style.ParseColor(v)
	if err != nil {
		t.Fatalf("colour %q: %v", v, err)
	}
	if c.IsDefault() {
		return tuicore.CellColor{}
	}
	if i, ok := c.ANSIIndex(); ok {
		return tuicore.CellColor{Kind: tuicore.CellColorANSI, Index: uint8(i)}
	}
	if i, ok := c.ANSI256Index(); ok {
		return tuicore.CellColor{Kind: tuicore.CellColorANSI256, Index: uint8(i)}
	}
	r, g, b, ok := c.RGBValues()
	if !ok {
		t.Fatalf("colour %q is neither default, ANSI nor RGB", v)
	}
	return color(r, g, b)
}

// themeColor is one of a theme's colours, by its dotted name ("document.base"), as a cell wears it.
func themeColor(t *testing.T, theme, key string) tuicore.CellColor {
	t.Helper()
	vals, err := themes.Values(theme)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := vals[key]
	if !ok {
		t.Fatalf("%s has no %s", theme, key)
	}
	return cellColorOf(t, v)
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
	h.RunCommand("options.theme.retro") // these assertions read the Retro palette's roles
	s.WaitFor(t, "retro palette", func(string) bool { return h.Theme() == "retro" })
	s.WaitForText(t, "▸ main (1)")
	retro := func(key string) tuicore.CellColor { return themeColor(t, "retro", key) }
	blurred := paintedLabel(t, s, "main (1)")
	blurredCorner := s.Backend.Snapshot()[1][0].Attrs
	if blurred.FG != retro("app.inactive.highlightedText") || blurred.BG != retro("app.inactive.highlight") || blurred.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("blurred explorer cursor did not use the inactive palette: %+v", blurred)
	}
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "explorer has focus", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	s.WaitFor(t, "selected row looks focused", func(string) bool {
		return paintedLabel(t, s, "main (1)") != blurred
	})
	focused := paintedLabel(t, s, "main (1)")
	if focused.FG != retro("document.highlightedText") || focused.BG != retro("document.highlight") || focused.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("focused explorer cursor has the wrong color/Reverse mask: %+v", focused)
	}
	// the panel's frame is golib's Panel's: focused, its border wears the Window's highlight
	if corner := s.Backend.Snapshot()[1][0].Attrs; corner.FG != retro("app.highlight") || corner.BG != blurredCorner.BG || corner.Mask&tuicore.AttrReverse != 0 {
		t.Fatalf("focused frame did not brighten independently of the row: %+v", corner)
	}
	s.Keys(t, enter())
	s.WaitForText(t, "connections")
	if normal := paintedLabel(t, s, "connections"); normal.FG != retro("document.text") || normal.BG != retro("document.base") || normal.Mask&tuicore.AttrReverse != 0 {
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

func TestEveryThemesFocusedExplorerCursorIsItsDocumentCursor(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, ctrl('h'))
	s.WaitFor(t, "explorer focus", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	// each theme's own document cursor, as golib writes it: the colours are golib's, not this test's
	for _, name := range tuiapp.ThemeNames() {
		vals, err := themes.Values(name)
		if err != nil {
			t.Fatal(err)
		}
		tc := struct {
			name   string
			fg, bg tuicore.CellColor
		}{name, cellColorOf(t, vals["document.highlightedText"]), cellColorOf(t, vals["document.highlight"])}
		h.RunCommand("options.theme." + tc.name)
		s.WaitFor(t, tc.name+" rendered explorer cursor", func(sc string) bool {
			if h.Theme() != tc.name || h.PaneWithFocus() != "explorerTree" || !strings.Contains(sc, "main (1)") {
				return false
			}
			cell := paintedLabel(t, s, "main (1)")
			return cell.FG == tc.fg && cell.BG == tc.bg && cell.Mask&tuicore.AttrReverse == 0
		})
	}
}

func TestMonoMenuMnemonicInheritsBothNormalAndSelectedText(t *testing.T) {
	h, s := signedIn(t)
	h.RunCommand("options.theme.mono")
	s.WaitFor(t, "mono menu", func(sc string) bool { return h.Theme() == "mono" && strings.Contains(sc, "View") })
	viewX := strings.Index(strings.Split(s.String(), "\n")[0], "View")
	if viewX < 0 {
		t.Fatal("View menu title missing")
	}
	menuCells := func() (tuicore.CellAttrs, tuicore.CellAttrs) {
		row := s.Backend.Snapshot()[0]
		return row[viewX].Attrs, row[viewX+1].Attrs
	}
	key, text := menuCells()
	if key.FG != text.FG || key.BG != text.BG || key.Mask&tuicore.AttrUnderline == 0 {
		t.Fatalf("normal Mono mnemonic lost its row colour or underline: key %+v, text %+v", key, text)
	}
	normal := key
	s.Keys(t, decltest.Alt('v'))
	s.WaitForText(t, "Zoom")
	key, text = menuCells()
	if key.FG != text.FG || key.BG != text.BG || key.Mask&tuicore.AttrUnderline == 0 {
		t.Fatalf("selected Mono mnemonic lost its row colour or underline: key %+v, text %+v", key, text)
	}
	if key.FG == normal.FG && key.BG == normal.BG {
		t.Fatalf("opening View did not invert the selected menu title: %+v", key)
	}
}
