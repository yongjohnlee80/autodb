package tui

import (
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Context help (Johno, M6 manual testing): `?` shows the keys available
// RIGHT HERE — the open modal's actions, or the focused panel's — in a
// float anchored to the bottom-right corner. One source of truth: the
// same hint list renders in a manager's footer and in this overlay.

// keyHint is one binding: the key and what it does.
type keyHint struct{ key, label string }

// hintProvider is implemented by float bodies that own key actions.
type hintProvider interface{ hints() []keyHint }

// hintCells renders hints as "k:label" cells for a footer line.
func hintCells(hs []keyHint) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.key + ":" + h.label
	}
	return out
}

// currentHints resolves the context: the topmost open float that owns
// actions, else the focused panel.
func (m *Model) currentHints() (title string, hs []keyHint) {
	for i := len(m.floats) - 1; i >= 0; i-- {
		f := m.floats[i]
		if !f.f.Shown() {
			continue
		}
		if hp, ok := f.body.(hintProvider); ok {
			return f.title, hp.hints()
		}
		// A float without its own actions (form, help) still traps keys:
		// report it rather than the panel behind it.
		return f.title, []keyHint{
			{"Tab", "next field"}, {"Enter", "submit"}, {"Esc", "cancel"},
		}
	}
	switch {
	case m.ctx.FocusWithin(m.explorerBox):
		return "explorer", []keyHint{
			{"j/k", "down / up"}, {"l", "expand"}, {"h", "collapse / parent"},
			{"g/G", "first / last"}, {"Enter", "scaffold a query for the table"},
			{"a", "add a connection / note here"}, {"d", "delete the note"},
			{"/", "search"}, {"n/N", "next / previous match"},
			{"SPC", "commands"},
		}
	case m.ctx.FocusWithin(m.resultsBox):
		return "results", []keyHint{
			{"j/k", "down / up"}, {"g/G", "first / last"},
			{"v", "inspect the row"}, {"Enter", "inspect the row"},
			{"/", "search"}, {"n/N", "next / previous match"},
			{"SPC j", "table / JSON toggle"}, {"SPC", "commands"},
		}
	default:
		return "query editor", []keyHint{
			{"i/a/o", "insert"}, {"jk", "escape to Normal"},
			{"hjkl", "move"}, {"w/b/e", "word motions"}, {"0/$", "line ends"},
			{"v/V", "visual"}, {"y/x/p", "yank / delete / paste"},
			{"dd/yy", "line delete / yank"}, {"u / Ctrl-r", "undo / redo"},
			{"/", "search"}, {"n/N", "next / previous match"},
			{"SPC r", "run the query"}, {"SPC", "commands"},
		}
	}
}

// openHints shows the context keys in the bottom-right corner.
func (m *Model) openHints() {
	title, hs := m.currentHints()
	body := &hintPanel{hints: hs}
	body.float = m.openFloatAt(title+" — keys", body, hintWidth, widget.BottomRight)
}

// hintPanel renders "key  label" rows.
type hintPanel struct {
	widget.Base
	hints []keyHint
	float *widget.Float
}

func (h *hintPanel) AcceptsFocus() bool { return true }

func (h *hintPanel) Layout(c tui.Constraints) tui.Size {
	return c.Constrain(tui.Size{
		W: min(c.MaxW, hintWidth-2),
		H: min(c.MaxH, len(h.hints)),
	})
}

func (h *hintPanel) Render(s tui.Surface) {
	keySt := style.New().Foreground(style.TokenPrimary).Bold(true)
	labelSt := style.New()
	width := 0
	for _, e := range h.hints {
		width = max(width, len(e.key))
	}
	for i, e := range h.hints {
		if i >= s.Size().H {
			break
		}
		drawTo(s, 0, i, e.key, keySt)
		drawTo(s, width+2, i, e.label, labelSt)
	}
}

// Any key dismisses (it is a reference card, not a menu); the key that
// dismissed it is consumed so `?` toggles cleanly.
func (h *hintPanel) HandleEvent(ev tui.Event) bool {
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease {
		h.float.Hide()
		return true
	}
	return false
}

// hintLine renders the hints as a single wrapped footer string.
func hintLine(hs []keyHint) string { return strings.Join(hintCells(hs), "  ") }
