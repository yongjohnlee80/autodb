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
	// AND IT CARRIES NO FILL. The chip was tried and removed: a filled block
	// behind two short words, wider than the word, read as a defect rather
	// than as emphasis on an otherwise unfilled card.
	if lab.BG.Kind != tuicore.CellColorDefault {
		t.Errorf("the input label has a background fill (%v); bold alone is the separation", lab.BG.Kind)
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

// THE FOOTER BAND READS TOP TO BOTTOM: rule, buttons, keys.
//
// The hints used to render ABOVE the buttons, because golib's modalCard lays
// out title, body and buttons in that order and the hints are in the body. The
// row is owned by the body now, which is what lets a key list sit beneath the
// controls it describes rather than above them.
func TestChrome_TheFooterBandIsRuleThenButtonsThenKeys(t *testing.T) {
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
	button, ok := rowOf(lines, "OK")
	if !ok {
		t.Fatalf("the OK button is not on screen:\n%s", h.screen())
	}
	keys, ok := rowOf(lines, "Tab:next")
	if !ok {
		t.Fatalf("the key hints are not on screen:\n%s", h.screen())
	}

	// A run of box-drawing dashes strictly between the title and the buttons
	// can only be the rule: the card's own borders lie outside that span.
	rule := -1
	for y := title + 1; y < button; y++ {
		if strings.Contains(lines[y], strings.Repeat("─", 8)) {
			rule = y
			break
		}
	}
	if rule < 0 {
		t.Fatalf("no rule between the body and the buttons:\n%s", h.screen())
	}
	if !(rule < button && button < keys) {
		t.Fatalf("the footer band is out of order: rule=%d buttons=%d keys=%d\n%s",
			rule, button, keys, h.screen())
	}
}

// THE FOOTER DOES NOT NAME A KEY THAT DOES NOT WORK. It went on advertising
// `O:OK` after the mnemonic was removed from the affirmative button — in the
// same function whose comment says a footer doing that teaches a key that does
// not work.
func TestChrome_TheFooterDoesNotAdvertiseTheRemovedMnemonic(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	if strings.Contains(h.screen(), "O:OK") {
		t.Fatalf("the footer still advertises the removed `O` mnemonic:\n%s", h.screen())
	}
	// AND PRESSING IT DOES NOTHING, which is the behaviour the footer was
	// misdescribing. A cell asserting only the text would pass for a form that
	// had quietly kept the binding.
	h.key('O')
	h.settle()
	var open bool
	h.on(func() { open = h.m.modalOpen() })
	if !open {
		t.Fatal("`O` submitted the form; it is a character in a field, not an accelerator")
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

// AN EMPTY FIELD LOOKS LIKE A FIELD. On the card's own background a blank
// input is indistinguishable from a blank line, so the thing the operator is
// meant to type into reads as a gap — which is most of why the connection form
// was reported as hard to fill in.
func TestChrome_AnEmptyInputShowsItsPlaceholder(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	if !strings.Contains(h.screen(), emptyPlaceholder) {
		t.Fatalf("an empty input shows nothing at all:\n%s", h.screen())
	}
}

// THE MARKER FOLLOWS THE KEYBOARD, AND IT SITS ON THE VALUE ROW.
//
// The reported confusion was on a form whose select gave no sign of holding
// focus: the operator did not know where they were or what Enter would do, and
// pressed it to find out. The pointer answers that — beside the place the value
// appears, not beside the label, which names the field rather than being it.
//
// Both rows are asserted at each step, because "the marker moved" and "a
// marker appeared" are different claims and only the first is wanted.
func TestChrome_TheFocusedRowIsMarked(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname"), field("port")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	// markerRow reports which screen row carries the pointer.
	markerRow := func(t *testing.T) int {
		t.Helper()
		lines := strings.Split(h.screen(), "\n")
		for y, l := range lines {
			if strings.Contains(l, strings.TrimSpace(markerFocused)) {
				return y
			}
		}
		t.Fatalf("no focus marker on screen:\n%s", h.screen())
		return -1
	}
	rowOfText := func(t *testing.T, want string) int {
		t.Helper()
		y, ok := rowOf(strings.Split(h.screen(), "\n"), want)
		if !ok {
			t.Fatalf("%q is not on screen:\n%s", want, h.screen())
		}
		return y
	}

	// THE MARKER IS BELOW ITS LABEL, which is what "on the value row" means.
	// A pointer beside the label would sit on the same row as the label text.
	first := markerRow(t)
	if lab := rowOfText(t, "hostname"); first != lab+1 {
		t.Fatalf("the marker is on row %d and the label \"hostname\" on row %d; "+
			"it belongs on the value row directly beneath", first, lab)
	}

	h.key(tuicore.KeyTab)
	h.settle()

	second := markerRow(t)
	if second == first {
		t.Fatalf("the marker did not follow Tab; still on row %d\n%s", first, h.screen())
	}
	if lab := rowOfText(t, "port"); second != lab+1 {
		t.Fatalf("after Tab the marker is on row %d and \"port\" on row %d", second, lab)
	}
	// AND ONLY ONE ROW CARRIES IT. A marker that appeared without the previous
	// one clearing would leave two rows claiming the keyboard.
	if n := strings.Count(h.screen(), strings.TrimSpace(markerFocused)); n != 1 {
		t.Fatalf("%d rows carry the focus marker, want exactly 1:\n%s", n, h.screen())
	}
}

// THE BUTTONS SIT AGAINST THE RIGHT EDGE, and a blank row separates them from
// the keys.
//
// Left-aligned, the button row and the hint line stacked into one column and
// read as a caption on the buttons rather than as the footer of the form.
func TestChrome_ButtonsAreRightAlignedAndSpacedFromTheKeys(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("endpoint", []formField{field("hostname")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	h.settle()

	lines := strings.Split(h.screen(), "\n")
	button, ok := rowOf(lines, "OK")
	if !ok {
		t.Fatalf("the OK button is not on screen:\n%s", h.screen())
	}
	keys, ok := rowOf(lines, "Tab:next")
	if !ok {
		t.Fatalf("the key hints are not on screen:\n%s", h.screen())
	}
	// A BLANK ROW BETWEEN THEM, which is what "spaced" means and what an empty
	// Text would have failed to reserve.
	if keys-button < 2 {
		t.Errorf("the keys are on row %d and the buttons on row %d; want a blank row between",
			keys, button)
	}

	// RIGHT-ALIGNED: the buttons end nearer the card's right edge than its
	// left, and further right than the left-aligned hint line begins.
	bl, br := strings.Index(lines[button], "["), strings.LastIndex(lines[button], "]")
	kl := strings.Index(lines[keys], "Tab:next")
	if bl < 0 || br < 0 || kl < 0 {
		t.Fatalf("could not locate the rows to compare:\n%s", h.screen())
	}
	if bl <= kl {
		t.Errorf("the button row starts at column %d and the hints at %d; "+
			"the buttons are not right-aligned", bl, kl)
	}
}
