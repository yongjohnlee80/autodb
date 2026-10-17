package autodocplugin

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	"github.com/yongjohnlee80/golib/tui/decl/themes"
	"github.com/yongjohnlee80/golib/tui/style"
)

// builtIn is AutoDoc's built-in theme name, as plugin.open sends it: its colours by dotted name.
func builtIn(t *testing.T, name string) plugin.Theme {
	t.Helper()
	colors, err := themes.Values(name)
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Theme{Name: name, Colors: colors}
}

// selection is the theme's selection colours as a run carries them.
func selection(th plugin.Theme) plugin.Style { return selectionWithFocus(th, false) }

func selectionWithFocus(th plugin.Theme, focused bool) plugin.Style {
	bgKey, fgKey := "app.inactive.highlight", "app.inactive.highlightedText"
	if focused {
		bgKey, fgKey = themeSelectionBG, themeSelectionFG
	}
	bg, fg := th.Colors[bgKey], th.Colors[fgKey]
	b, be := style.ParseColor(bg)
	f, fe := style.ParseColor(fg)
	if be == nil && fe == nil {
		if r, g, blue, ok := style.Legible(f, b, 4.5).RGBValues(); ok {
			fg = fmt.Sprintf("#%02x%02x%02x", r, g, blue)
		}
	}
	return plugin.Style{BG: bg, FG: fg}
}

// runOf is the run of the last frame's row y that holds text, and whether there is one.
func (h *fakeHost) runOf(text string) (y int, r plugin.Run, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for y, row := range h.frame.Rows() {
		for _, r := range row {
			if strings.Contains(r.Text, text) {
				return y, r, true
			}
		}
	}
	return -1, plugin.Run{}, false
}

// atPick is a plugin connected to scratch and back on the pick, ledger chosen, w×hh.
func atPick(t *testing.T, w, hh int) (*Plugin, *fakeHost) {
	t.Helper()
	db := newFakeDB(t)
	db.more = []map[string]any{{"id": int64(8), "name": "ledger", "engine": "sqlite"}}
	p, h := connected(t, db)
	p.Resize(w, hh)
	p.Key(plugin.Key{Key: "c"})
	p.Key(plugin.Key{Key: "j"})
	return p, h
}

// In every built-in theme the chosen connection is a bar of the theme's selection colours, the
// card's whole width inside its margins, its "> " mark kept; the focused field and the focused
// button wear the same colours, the field underlined. No raw palette colour is left.
func TestTheChosenRowIsTheThemesSelectionBar(t *testing.T) {
	names := themes.Names()
	for _, want := range []string{"sepia", "light", "dark", "retro", "mono"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the built-in themes are %v, missing %s", names, want)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			th := builtIn(t, name)
			sel := selection(th)
			if sel.BG == "" || sel.FG == "" {
				t.Fatalf("%s names no %s or %s", name, themeSelectionBG, themeSelectionFG)
			}
			const w = 32
			p, h := atPick(t, w, 16)
			p.Theme(th)
			_, r, ok := h.runOf("> ledger (sqlite)")
			if !ok {
				t.Fatalf("no chosen row:\n%s", strings.Join(h.lines(), "\n"))
			}
			if r.Style != sel {
				t.Errorf("the chosen row is %+v, want the selection %+v", r.Style, sel)
			}
			if n := len([]rune(r.Text)); n != w-2 || !strings.HasPrefix(r.Text, "> ") {
				t.Errorf("the chosen row's bar is %q, %d cells: want \"> \" first and the %d cells inside the margins", r.Text, n, w-2)
			}
			if _, other, _ := h.runOf("scratch (postgres) · current"); other.Style.BG != "" || other.Style.FG != "" {
				t.Errorf("the row not chosen is %+v, want the card's own colours", other.Style)
			}

			s, sh := atSignIn(t, newFakeDB(t), w, 16)
			s.Theme(th)
			_, field, _ := sh.runOf("█")
			if want := (plugin.Style{BG: sel.BG, FG: sel.FG, Underline: true}); field.Style != want {
				t.Errorf("the focused field is %+v, want %+v", field.Style, want)
			}
			tab(s)
			tab(s)
			if _, b, _ := sh.runOf("[ Sign in ]"); b.Style != sel {
				t.Errorf("Sign in, focused, is %+v, want %+v", b.Style, sel)
			}
			for _, c := range []*fakeHost{h, sh} {
				for _, row := range c.frame.Rows() {
					for _, r := range row {
						if r.Style.FG == "cyan" || r.Style.BG == "cyan" {
							t.Fatalf("a run in raw cyan: %+v", r)
						}
					}
				}
			}
		})
	}
}

// The card's cursor takes the document's active highlight and the app's inactive highlight, the
// same palette roles the results table uses. Both states keep a readable foreground.
func TestTheCardCursorMatchesTheResultsPaletteInBothFocusStates(t *testing.T) {
	for _, name := range themes.Names() {
		th := builtIn(t, name)
		p, h := atPick(t, 32, 16)
		p.Theme(th)
		for _, focused := range []bool{false, true} {
			p.Focus(focused)
			_, run, _ := h.runOf("> ") // the selected connection, in the cursor's style
			st := run.Style
			wantBG := th.Colors["app.inactive.highlight"]
			if focused {
				wantBG = th.Colors["document.highlight"]
			}
			fg, fgErr := style.ParseColor(st.FG)
			bg, bgErr := style.ParseColor(st.BG)
			ratio, ok := style.ContrastRatio(fg, bg)
			if st.BG != wantBG || fgErr != nil || bgErr != nil || !ok || ratio < 4.5 {
				t.Errorf("%s focused=%v: cursor %+v, wanted background %s and 4.5 contrast (got %.2f)", name, focused, st, wantBG, ratio)
			}
		}
	}
}

// A theme naming no selection colours (an older host, a theme of the user's) leaves the choice to
// bold and the "> " mark, and the focused field to bold and its underline: never a raw colour.
func TestAThemeWithoutSelectionColoursFallsBackToBold(t *testing.T) {
	for _, th := range []plugin.Theme{{}, {Name: "mine", Colors: map[string]string{"app.window": "#101010", themeSelectionBG: "#202020"}}} {
		p, h := atPick(t, 32, 16)
		p.Theme(th)
		_, r, ok := h.runOf("> ledger (sqlite)")
		if !ok || r.Style != stChosen || stChosen != (plugin.Style{Bold: true}) {
			t.Fatalf("theme %+v: the chosen row is %+v, want bold alone", th, r)
		}
		s, sh := atSignIn(t, newFakeDB(t), 32, 16)
		s.Theme(th)
		if _, f, _ := sh.runOf("█"); f.Style != (plugin.Style{Bold: true, Underline: true}) {
			t.Fatalf("theme %+v: the focused field is %+v, want bold and underlined", th, f.Style)
		}
	}
}

// A theme switch recolours the card at once: the bar takes the new theme's selection.
func TestAThemeSwitchRecolours(t *testing.T) {
	p, h := atPick(t, 32, 16)
	p.Theme(builtIn(t, "sepia"))
	frames := h.frames
	dark := builtIn(t, "dark")
	p.Theme(dark)
	if h.frames != frames+1 {
		t.Fatalf("a theme switch sent %d frames, want one", h.frames-frames)
	}
	if _, r, _ := h.runOf("> ledger (sqlite)"); r.Style != selection(dark) {
		t.Fatalf("after the switch to dark the bar is %+v, want %+v", r.Style, selection(dark))
	}
}

// picture is the last frame as ASCII: each row's text between bars, then its cells marked # where
// they wear sel, _ where underlined, and . elsewhere. The server's port is masked.
func picture(h *fakeHost, sel plugin.Style) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	port := regexp.MustCompile(`127\.0\.0\.1:\d{5}`)
	var b strings.Builder
	for _, row := range h.frame.Rows() {
		var text, marks strings.Builder
		for _, r := range row {
			text.WriteString(r.Text)
			m := "."
			switch {
			case r.Style == sel:
				m = "#"
			case r.Style.Underline:
				m = "_"
			}
			marks.WriteString(strings.Repeat(m, len([]rune(r.Text))))
		}
		b.WriteString("|" + port.ReplaceAllString(text.String(), "127.0.0.1:PPPPP") + "|" + marks.String() + "|\n")
	}
	return b.String()
}

// The pick at 32 wide, in sepia and in dark: the bar runs the width inside the margins, under the
// connection and the heading, and nothing else wears it.
func TestThePickAsASCII(t *testing.T) {
	const want = `` +
		"| Connection: scratch (postgres) |................................|\n" +
		"| Choose a connection            |................................|\n" +
		"| AutoDB v-fake at               |................................|\n" +
		"| 127.0.0.1:PPPPP                |................................|\n" +
		"|                                |................................|\n" +
		"|   scratch (postgres) · current |................................|\n" +
		"| > ledger (sqlite)              |.##############################.|\n" +
		"|                                |................................|\n" +
		"|                                |................................|\n" +
		"| ────────────────────────────── |................................|\n" +
		"| ↑↓ j k: choose · Enter:        |................................|\n" +
		"| connect · q: close             |................................|\n" +
		"| connected to scratch           |................................|\n" +
		"| (postgres)                     |................................|\n"
	// The unfocused card uses the app's inactive highlight, not the old text-selection bar.
	for _, name := range []string{"sepia", "dark"} {
		th := builtIn(t, name)
		p, h := atPick(t, 32, 14)
		p.Theme(th)
		if got := picture(h, selection(th)); got != want {
			t.Errorf("%s: the pick is\n%s\nwant\n%s", name, got, want)
		}
	}
}

// Over AutoDoc's own SDK and link: plugin.open's theme colours the card from its first frame, and
// plugin.theme recolours it.
func TestTheThemeCrossesTheRealLink(t *testing.T) {
	db := newFakeDB(t)
	p := newPlugin("v0.0.1", t.TempDir())
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostEnd, pluginEnd := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- plugin.ServeConn(ctx, pluginEnd, p) }()
	got := make(chan note, 256)
	link, err := plugin.NewLink(ctx, hostEnd, func(method string, params []any) { got <- note{method, params} })
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	// fieldIn waits for a frame whose focused field wears want
	fieldIn := func(want plugin.Style) {
		t.Helper()
		// NOTE: a test's deadline: a plugin that never answers fails the cell rather than hang it.
		deadline := time.After(10 * time.Second)
		for {
			select {
			case n := <-got:
				if n.method != plugin.MethodFrame {
					continue
				}
				rows, _, err := plugin.ReadFrame(n.params)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					for _, r := range row {
						if strings.Contains(r.Text, "█") && r.Style == want {
							return
						}
					}
				}
			case <-deadline:
				t.Fatalf("no frame with the focused field in %+v within 10 s", want)
			}
		}
	}
	sepia, dark := builtIn(t, "sepia"), builtIn(t, "dark")
	field := func(th plugin.Theme) plugin.Style {
		st := selectionWithFocus(th, true)
		st.Underline = true
		return st
	}
	if err := link.Notify(ctx, plugin.MethodOpen, plugin.OpenParams(plugin.Open{Protocol: 4, Width: 32, Height: 16, Theme: sepia})); err != nil {
		t.Fatal(err)
	}
	if err := link.Notify(ctx, plugin.MethodFocus, plugin.FocusParams(true)); err != nil {
		t.Fatal(err)
	}
	fieldIn(field(sepia))
	if err := link.Notify(ctx, plugin.MethodTheme, plugin.ThemeParams(dark)); err != nil {
		t.Fatal(err)
	}
	fieldIn(field(dark))
	if err := link.Notify(ctx, plugin.MethodClose, plugin.EmptyParams()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after plugin.close")
	}
}
