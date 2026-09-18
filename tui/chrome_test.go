package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// attrsOf finds the first row containing want and returns the attributes of
// the cell under its first character, plus the row index.
func attrsOf(t *testing.T, h *barHarness, want string) (tuicore.CellAttrs, int) {
	t.Helper()
	grid := h.tb.Snapshot()
	for y, row := range grid {
		var b strings.Builder
		for _, c := range row {
			if c.Content == "" {
				continue
			}
			b.WriteString(c.Content)
		}
		line := b.String()
		col := strings.Index(line, want)
		if col < 0 {
			continue
		}
		// Walk the row again to map the rune offset back onto a cell.
		seen := 0
		for x, c := range row {
			if c.Content == "" {
				continue
			}
			if seen == col {
				return row[x].Attrs, y
			}
			seen += len(c.Content)
		}
	}
	t.Fatalf("%q is not on screen:\n%s", want, h.screen())
	return tuicore.CellAttrs{}, -1
}

// A HINT IS FAINT, AND THAT IS NOT A SPELLING DETAIL.
//
// golib DERIVES TokenTextMuted from TokenForeground: the token carries the
// de-emphasized colour and nothing else, so on a default theme it IS the
// foreground. Every hint line in this application spelled the muted look as
// Foreground(TokenTextMuted) alone and therefore rendered at full brightness,
// competing with the content it was meant to sit behind.
func TestChrome_HintTextIsFaint(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("name it", []formField{field("name")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	at, _ := attrsOf(t, h, "Tab")
	if at.Mask&tuicore.AttrFaint == 0 {
		t.Fatal("the key hints render at full brightness; they are chrome and must recede")
	}
}

// A LABEL IS BOLD ON AN OFFSET BACKGROUND, and the VALUE is what carries the
// underline. The two were the wrong way round: underlining the label decorated
// the word the operator is not editing and left the field they are editing
// with no edge at all.
func TestChrome_LabelIsBoldChipAndValueIsUnderlined(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	typeText(h, "elsewhere")
	h.settle()

	lab, _ := attrsOf(t, h, "hostname")
	if lab.Mask&tuicore.AttrBold == 0 {
		t.Error("the input label is not bold")
	}
	if lab.BG.Kind == tuicore.CellColorDefault {
		t.Error("the input label has no background; it should read as a chip of chrome")
	}
	if lab.Mask&tuicore.AttrUnderline != 0 {
		t.Error("the LABEL is underlined; the underline belongs on the value")
	}

	val, _ := attrsOf(t, h, "elsewhere")
	if val.Mask&tuicore.AttrUnderline == 0 {
		t.Error("the typed value is not underlined; nothing marks the editable region")
	}
}

// THE BUTTONS ARE THE SAME WIDTH. "OK" beside "Cancel" rendered as a stub next
// to a slab, which reads as one real control and one afterthought — and the
// affirmative was the stub.
func TestChrome_ButtonsAreEqualWidth(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("name it", []formField{field("name")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	// padButtonLabels("OK","Cancel") widens OK to six columns, two either side.
	if !strings.Contains(h.screen(), "  OK  ") {
		t.Fatalf("the affirmative was not widened to match Cancel:\n%s", h.screen())
	}
}

func TestChrome_PadButtonLabels(t *testing.T) {
	for _, tc := range []struct {
		what string
		in   []string
		want []string
	}{
		{"the short one is centred", []string{"OK", "Cancel"}, []string{"  OK  ", "Cancel"}},
		{"an odd remainder goes right", []string{"No", "Yes!"}, []string{" No ", "Yes!"}},
		{"already equal, untouched", []string{"Yes", "No!"}, []string{"Yes", "No!"}},
		{"three answers all match the widest", []string{"a", "bb", "ccc"}, []string{" a ", "bb ", "ccc"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := padButtonLabels(tc.in)
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// THE RULE SEPARATES THE OPERATOR'S DATA FROM THE CHROME, and the buttons sit
// below a line rather than below a sentence. golib's modal card has no footer
// of its own, so without this the key hints render as one more line of the
// modal's content, indistinguishable from the prose above them.
func TestChrome_ARuleSitsBetweenTheBodyAndTheButtons(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	lines := strings.Split(h.screen(), "\n")
	title, ok := rowOf(lines, "endpoint")
	if !ok {
		t.Fatalf("the modal title is not on screen:\n%s", h.screen())
	}
	button, ok := rowOf(lines, "  OK  ")
	if !ok {
		t.Fatalf("the OK button is not on screen:\n%s", h.screen())
	}
	// A run of box-drawing dashes strictly between the title and the buttons
	// can only be the rule: the card's own borders lie outside that span.
	found := false
	for y := title + 1; y < button; y++ {
		if strings.Contains(lines[y], strings.Repeat("─", 8)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no rule between the body and the buttons:\n%s", h.screen())
	}
}

func rowOf(lines []string, want string) (int, bool) {
	for y, l := range lines {
		if strings.Contains(l, want) {
			return y, true
		}
	}
	return 0, false
}

// THE BUILD IDENTITY OWNS THE BOTTOM-RIGHT CORNER UNTIL SOMEBODY SIGNS IN.
// It is the question an operator has in front of a login prompt on a host they
// just provisioned, and About — behind a menu, after sign-in — answers it too
// late.
func TestChrome_BuildLineShowsUntilSignIn(t *testing.T) {
	h := startBarWith(t, "", true)
	h.on(func() {
		h.m.session.user = UserInfo{}
		// A STATUS MESSAGE IS PRESENT, deliberately. On the surface this
		// exists for -- a freshly provisioned host -- the right slot is
		// occupied by "connecting to ..." for the whole life of the login
		// prompt, so a build line that only took an EMPTY slot never appeared
		// on the one screen it was added for.
		h.m.statusMsg = "connecting to 127.0.0.1:1…"
		h.m.about = AboutInfo{
			Version: "v0.3.15", Commit: "f011ce5",
			BuildDate: "2026-09-18", Author: "John Lee",
		}
		h.m.refreshStatus()
	})
	h.settle()
	for _, want := range []string{"v0.3.15", "f011ce5", "2026-09-18", "John Lee"} {
		if !strings.Contains(h.screen(), want) {
			t.Fatalf("%q is not on the pre-sign-in backdrop:\n%s", want, h.screen())
		}
	}

	// AND IT YIELDS. Once there is an identity the corner belongs to the work.
	h.on(func() {
		h.m.session.user = UserInfo{ID: 1, Name: "op", Role: meta.RoleAdmin}
		h.m.refreshStatus()
	})
	h.settle()
	if strings.Contains(h.screen(), "f011ce5") {
		t.Fatalf("the build line survived sign-in:\n%s", h.screen())
	}
}

// AN UNSTAMPED BUILD SAYS NOTHING. "dev"/"none"/"unknown" look like a version
// and are not one, so printing them tells the reader less than printing
// nothing.
func TestChrome_BuildLine(t *testing.T) {
	for _, tc := range []struct {
		what string
		in   AboutInfo
		want string
	}{
		{"fully stamped", AboutInfo{Version: "v1.2.3", BuildDate: "2026-09-18", Commit: "abc1234", Author: "John Lee"},
			"autodb v1.2.3 ⋅ 2026-09-18 ⋅ abc1234 ⋅ John Lee"},
		{"no version at all", AboutInfo{Commit: "abc1234"}, ""},
		{"an unstamped dev build", AboutInfo{Version: "dev", Commit: "none", BuildDate: "unknown"}, ""},
		{"placeholders dropped, version kept", AboutInfo{Version: "v1.2.3", Commit: "none", BuildDate: "unknown"},
			"autodb v1.2.3"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if got := buildLine(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
