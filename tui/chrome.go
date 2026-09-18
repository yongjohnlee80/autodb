package tui

// Shared modal chrome: the one place that decides how an input label looks and
// how wide a dialog's buttons are.
//
// Both were per-call-site decisions and both drifted. Labels were built with a
// muted style repeated in three files, so a change had to be made three times
// and was made once; buttons were sized by their text, so "OK" beside "Cancel"
// rendered as a stub next to a slab and the pair read as one important control
// and one afterthought.

import (
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// inputLabelStyle is how EVERY text-input label in this application is drawn:
// bold, on a slightly offset background, so the prompt reads as a chip of
// chrome rather than as another line of the modal's prose.
//
// THE UNDERLINE IS ON THE VALUE, NOT HERE — see inputValueStyles. Underlining
// the label decorated the word the operator is not editing and left the field
// they are editing with no edge at all; the two were the wrong way round.
//
// THE GRAY IS EXPLICIT, and TokenBoost is the trap it avoids. Boost reads like
// "a slightly offset surface" and derives from TokenSurface, which derives from
// TokenBackground, which is the terminal default — so on any theme that has not
// overridden the chain it resolves to no chip at all. The label came out bold
// on nothing. ANSI(8) is the palette's gray and is a gray on every terminal.
//
// It is a function rather than a package var because style.Style is a value
// with a builder API — a shared var would hand every caller the same value to
// keep modifying, and a caller that added one attribute would be editing a
// copy while reading like it had changed the shared look.
func inputLabelStyle() style.Style {
	return style.New().Foreground(style.TokenForeground).Background(style.ANSI(8)).Bold(true)
}

// inputValueStyles underline what the operator types.
//
// The underline marks the EDITABLE REGION, which is the thing a form has to
// make obvious and the thing this one did not: a bare value on the card's own
// background is indistinguishable from a line of text until the cursor happens
// to be in it.
//
// An EMPTY field still has nothing to underline, because there is no value to
// draw. A placeholder is what fills that gap, and the placeholder carries the
// underline too so the region is visible before anything is typed.
func inputValueStyles() widget.TextInputStyles {
	return widget.TextInputStyles{
		Text:        style.New().Underline(true),
		Placeholder: mutedStyle().Underline(true),
	}
}

// padButtonLabels returns the labels padded to a common width.
//
// LITERAL SPACES, because golib's Button has no width option: it measures its
// own text and there is nothing to tell it otherwise. Padding the text is
// therefore not a workaround for a missing feature so much as the only handle
// the widget offers, and it is done HERE so the arithmetic exists once.
//
// The padding is split either side, remainder to the right, so a label stays
// centred in its button rather than sitting against the left edge with a gap
// after it.
//
// Measured with tui.StringWidth, NOT by counting runes: a label is a display
// string, and rune count is the wrong number the moment one is not ASCII.
func padButtonLabels(labels []string) []string {
	widest := 0
	for _, l := range labels {
		if w := tui.StringWidth(l); w > widest {
			widest = w
		}
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		short := widest - tui.StringWidth(l)
		if short <= 0 {
			out[i] = l
			continue
		}
		left := short / 2
		out[i] = strings.Repeat(" ", left) + l + strings.Repeat(" ", short-left)
	}
	return out
}

// buildLine is the build identity shown in the corner of the backdrop.
//
// Empty when there is nothing worth saying. A binary built without the ldflags
// stamp carries "dev"/"none"/"unknown", and printing those tells the reader
// less than printing nothing: it looks like a version and is not one.
func buildLine(a AboutInfo) string {
	if a.Version == "" || a.Version == "dev" {
		return ""
	}
	parts := []string{"autodb " + a.Version}
	if a.BuildDate != "" && a.BuildDate != "unknown" {
		parts = append(parts, a.BuildDate)
	}
	if a.Commit != "" && a.Commit != "none" && a.Commit != "unknown" {
		parts = append(parts, a.Commit)
	}
	if a.Author != "" {
		parts = append(parts, a.Author)
	}
	return strings.Join(parts, " ⋅ ")
}

// hrule is a one-row horizontal divider that fills whatever width it is given.
//
// A CUSTOM WIDGET BECAUSE GOLIB HAS NONE, and because what a modal needs here
// is not a box border: a border would enclose a region, and this separates two
// bands of one card.
//
// It exists to give a modal a FOOTER. golib's modalCard lays out title, body
// and buttons, with no footer of its own, so a hint line placed in the body
// rendered as one more line of the modal's content -- indistinguishable from
// the prose above it and sitting immediately over the buttons. A rule as the
// last structural row turns everything after it into a band that reads as
// chrome, and puts the buttons below a line rather than below a sentence.
//
// NOT FOCUSABLE: it is a drawn line, and a tab stop on one would be a stop that
// does nothing.
type hrule struct {
	widget.Base
}

func newHRule() *hrule { return &hrule{} }

func (h *hrule) AcceptsFocus() bool { return false }

// Layout takes the full offered width and exactly one row. It asks for MaxW
// rather than a share of it: a divider that stopped short of the card's edge
// would read as an underline under the last field.
func (h *hrule) Layout(c tui.Constraints) tui.Size {
	return c.Constrain(tui.Size{W: c.MaxW, H: 1})
}

func (h *hrule) Render(s tui.Surface) {
	st := style.New().Foreground(style.TokenBorder)
	w := s.Size().W
	for x := range w {
		s.SetCell(x, 0, "─", st)
	}
}

func (h *hrule) HandleEvent(tui.Event) bool { return false }

// mutedStyle is de-emphasized text: hints, placeholders, the key glyphs in a
// footer — anything that should stay legible without competing with what the
// operator is actually doing.
//
// THE Faint IS THE WHOLE POINT, and its absence was a real defect repeated in
// six files. golib DERIVES TokenTextMuted from TokenForeground — the token
// carries the de-emphasized COLOR and nothing else, and on a default theme
// that color IS the foreground. So `Foreground(TokenTextMuted)` alone renders
// at full brightness, which is how every hint line in this application came to
// be the same white as the content it was supposed to sit behind. golib's own
// theme documentation spells the convention out: the muted look is
// Foreground(TokenTextMuted).Faint(true), and the second half was missing.
func mutedStyle() style.Style {
	return style.New().Foreground(style.TokenTextMuted).Faint(true)
}

// panelStyles is the border look of a framed panel: WHITE WHEN FOCUSED, FAINT
// OTHERWISE.
//
// golib's defaults are the other way about in effect. The unfocused border is
// TokenBorder, which derives from TokenForeground and so renders at the same
// brightness as the content; the focused one is TokenBorderFocused, which
// derives from the accent — blue on the default theme, and a light blue is
// LESS pronounced than white, not more. The result was four panels of equal
// weight with the active one tinted rather than picked out.
//
// Faint(false) on the focused style is load-bearing: the focused style is
// MERGED over the base, so without explicitly turning it off the base's faint
// would survive and the focused panel would be a faint white instead of a
// white one.
func panelStyles() (base, focused style.Style) {
	base = style.New().BorderForeground(style.TokenBorder).Faint(true)
	focused = style.New().BorderForeground(style.ANSI(15)).Faint(false).Bold(true)
	return base, focused
}
