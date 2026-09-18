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
// bold, and nothing else.
//
// IT CARRIED A GRAY CHIP AND THE CHIP IS GONE. A filled background behind two
// short words put a hard-edged block in the middle of an otherwise unfilled
// card, and the block was wider than the word in a way that read as a defect
// rather than as emphasis. Bold alone separates the prompt from the value
// under it, which is all the separation a two-row field needs.
//
// THE UNDERLINE IS ON THE VALUE, NOT HERE — see inputValueStyles. Underlining
// the label decorated the word the operator is not editing and left the field
// they are editing with no edge at all; the two were the wrong way round.
//
// It is a function rather than a package var because style.Style is a value
// with a builder API — a shared var would hand every caller the same value to
// keep modifying, and a caller that added one attribute would be editing a
// copy while reading like it had changed the shared look.
func inputLabelStyle() style.Style {
	return style.New().Foreground(style.TokenForeground).Bold(true)
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

// Render draws the line MUTED, which is the same correction the hint lines
// needed and for the same reason: TokenBorder derives from TokenForeground, so
// a rule asking for the border colour and nothing else comes out at the
// brightness of the content it is separating. A divider that competes with
// what it divides is not a divider.
//
// It spans whatever width it is given, which is the width the surrounding Flex
// hands it rather than the card's -- so it can stop short of the border. Left
// as it is deliberately: a rule that ran edge to edge would have to be drawn
// by the card, and the card is golib's.
func (h *hrule) Render(s tui.Surface) {
	st := mutedStyle()
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
	// AN EXPLICIT GRAY, NOT Faint. The unfocused border first asked for
	// TokenBorder with Faint(true), and came out white: TokenBorder derives
	// from TokenForeground, and the Box draws its frame from the border COLOUR
	// without carrying the style's text attributes, so the faint was never
	// applied to a single border cell. Naming the gray is the only spelling
	// that survives both.
	base = style.New().BorderForeground(style.ANSI(8))
	focused = style.New().BorderForeground(style.ANSI(15)).Bold(true)
	return base, focused
}

// listStyles is how a row list paints its cursor, and it comes in two forms
// because a modal with more than one list has to say WHICH one the keyboard is
// in.
//
// focused: black on cyan. unfocused: cyan on gray. The pair reads as one
// active section and one remembered position, rather than as two equal
// selections with no way to tell which keystrokes will reach.
//
// ALL FOUR FIELDS ARE SET, AND CursorSelected IS THE ONE THAT MATTERED. golib
// derives its default CursorSelected from its own default CursorRow at
// construction, so a caller that overrode CursorRow alone left CursorSelected
// holding the framework's inverted default -- and the row under the cursor in
// these managers is ALSO the selected row, so that stale default is what
// actually rendered. The cyan was configured and never drawn; what appeared
// was an inverted slate bar, which is why the selected workspace looked like a
// row in a list that had lost focus.
func listStyles(focused bool) widget.ListStyles {
	var cursor style.Style
	if focused {
		cursor = style.New().Background(style.ANSI(6)).Foreground(style.ANSI(0))
	} else {
		cursor = style.New().Background(style.ANSI(8)).Foreground(style.ANSI(6))
	}
	return widget.ListStyles{
		Row:            style.New(),
		CursorRow:      cursor,
		SelectedRow:    cursor,
		CursorSelected: cursor,
	}
}

// emptyPlaceholder is what an untouched text field shows.
//
// It is not decoration: an empty input on the card's own background is
// indistinguishable from a blank line, so the field the operator is meant to
// type into looks like a gap. The placeholder is muted and underlined, which
// draws the editable region before there is any value to underline.
const emptyPlaceholder = "empty…"

// buttonStyle is how EVERY button in this application is drawn.
//
// CYAN MEANS THE KEYBOARD IS HERE, which is the same thing it means on a list
// row, so one colour answers "where am I?" everywhere rather than two
// vocabularies the operator has to hold at once.
//
// AND AN UNFOCUSED BUTTON RECEDES. golib's default normal look is
// TokenSurface behind TokenForeground, and its focused look is that REVERSED
// -- which on a theme that leaves both as the terminal's own defaults comes
// out as a near-white block. Every button on screen therefore read as
// highlighted, including the ones nothing was pointing at, so the one that
// Enter would actually press was indistinguishable from the rest.
func buttonStyle() *widget.ButtonStyle {
	normal := style.New().Foreground(style.ANSI(8))
	focused := style.New().Background(style.ANSI(6)).Foreground(style.ANSI(0)).Bold(true)
	return widget.NewButtonStyle(normal, focused)
}
