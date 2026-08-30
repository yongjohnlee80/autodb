package tui

import (
	"context"
	"iter"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Script history (SPC H): who ran what, when, against which connection,
// and how it ended. The server decides what the caller may see — admins
// get everything, everyone else their own executions.
//
// The table ellipsizes the script; Enter opens the full text in a
// read-only vim viewer, the same move as inspecting a result row.

type historyView struct {
	widget.Base
	model *Model
	rows  []HistoryRow
	table *widget.Table[HistoryRow]
	hint  *widget.Text
	ctx   *tui.Context
	float *widget.Float
}

func newHistoryView(m *Model, rows []HistoryRow) *historyView {
	cols := []widget.TableColumn[HistoryRow]{
		{Title: "WHEN", Width: 20, Cell: func(r HistoryRow) string { return whenText(r.StartedAt) }},
		{Title: "WHO", Width: 12, Cell: func(r HistoryRow) string { return r.User }},
		{Title: "CONNECTION", Width: 14, Cell: func(r HistoryRow) string { return r.Conn }},
		{Title: "STATUS", Width: 9, Cell: func(r HistoryRow) string { return r.Status }},
		{Title: "ROWS", Width: 6, Cell: func(r HistoryRow) string {
			return strconv.FormatInt(r.RowCount, 10)
		}},
		{Title: "TOOK", Width: 8, Cell: func(r HistoryRow) string { return r.Duration.String() }},
		// Flex: whatever is left goes to the script preview.
		{Title: "SCRIPT", Cell: func(r HistoryRow) string {
			return strings.Join(strings.Fields(r.Script), " ")
		}},
	}
	v := &historyView{model: m, rows: rows}
	v.table = widget.NewTable(cols,
		widget.WithItems(rows, func(r HistoryRow) string { return r.Script }),
		widget.WithListStyles[HistoryRow](widget.ListStyles{CursorRow: cursorRowStyle}))
	v.hint = widget.NewText(hintLine(v.hints()),
		widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted)),
		widget.WithWrapMode(widget.Wrap))
	return v
}

// whenText trims the RFC3339 timestamp to something readable in a cell.
func whenText(ts string) string {
	if len(ts) >= 19 {
		return ts[:10] + " " + ts[11:19]
	}
	return ts
}

func (v *historyView) hints() []keyHint {
	return []keyHint{
		{"j/k", "down / up"}, {"Enter", "show the full script"},
		{"y", "copy the script to the editor register"},
		{"e", "load the script into the query editor"}, {"q/Esc", "close"},
	}
}

func (v *historyView) AcceptsFocus() bool { return true }

func (v *historyView) Init(ctx *tui.Context) {
	v.Base.Init(ctx)
	v.ctx = ctx
	ctx.Mount(v.table)
	ctx.Mount(v.hint)
}

func (v *historyView) selected() (HistoryRow, bool) {
	i, ok := v.table.List().Selected()
	if !ok || i >= len(v.rows) {
		return HistoryRow{}, false
	}
	return v.rows[i], true
}

func (v *historyView) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	if dismissKey(ev) {
		v.float.Hide()
		return true
	}
	switch {
	case k.Code == tui.KeyEnter:
		if r, ok := v.selected(); ok {
			v.model.openScript(scriptTitle(r), r.Script)
		}
		return true
	case k.Text == "y":
		if r, ok := v.selected(); ok {
			v.model.editor.SetRegister(r.Script, false)
			v.model.setStatus("script copied to the editor register (p to paste)")
			v.float.Hide()
		}
		return true
	case k.Text == "e":
		if r, ok := v.selected(); ok {
			v.model.loadScaffold(r.Script)
			v.float.Hide()
		}
		return true
	}
	return v.table.HandleEvent(ev)
}

func (v *historyView) Layout(c tui.Constraints) tui.Size {
	// The float owns the size (WithSizeFraction); the view fills it.
	w, h := c.MaxW, c.MaxH
	hintH := max(v.ctx.LayoutChild(v.hint, tui.Constraints{MaxW: w, MaxH: 3}).H, 1)
	tableH := max(h-hintH, 1)
	v.ctx.LayoutChild(v.table, tui.Tight(tui.Size{W: w, H: tableH}))
	v.ctx.PlaceChild(v.table, tui.Rect{X: 0, Y: 0, W: w, H: tableH})
	v.ctx.PlaceChild(v.hint, tui.Rect{X: 0, Y: tableH, W: w, H: hintH})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (v *historyView) Render(tui.Surface) {}

func (v *historyView) Add(...tui.Component) {}
func (v *historyView) Remove(tui.Component) {}

// Move is a no-op — fixed shape, nothing to permute (see connPicker.Move).
func (v *historyView) Move(tui.Component, int) {}
func (v *historyView) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		if v.table != nil {
			yield(v.table)
		}
	}
}

var _ tui.Container = (*historyView)(nil)

func scriptTitle(r HistoryRow) string {
	title := whenText(r.StartedAt) + " · " + r.User
	if r.Conn != "" {
		title += " · " + r.Conn
	}
	if r.Status != "" {
		title += " · " + r.Status
	}
	return title
}

// openHistory fetches and shows the script history.
func (m *Model) openHistory() {
	bound := m.session.Bind()
	m.setStatus("loading script history…")
	m.ctx.Go(func(c context.Context) (any, error) {
		rows, err := bound.History(c, 200)
		return managerReload{gen: bound.Gen(), apply: func() {
			if err != nil {
				m.setStatus("history: " + WireErrorMessage(err))
				return
			}
			if len(rows) == 0 {
				m.setStatus("no script history yet")
				return
			}
			m.statusMsg = ""
			v := newHistoryView(m, rows)
			v.float = m.openFloatPct("script history", v, historyPct)
		}}, nil
	})
}

// openScript shows one recorded script in a read-only vim viewer —
// navigable and yankable, never editable (same contract as the JSON
// results view).
func (m *Model) openScript(title, script string) {
	sv := &scriptView{model: m, text: script}
	// Smaller than the history behind it, so the stack reads as a
	// detail ON a list rather than a replacement for it.
	sv.float = m.openFloatPct(title, sv, scriptPct)
}

type scriptView struct {
	widget.Base
	model  *Model
	text   string
	editor *widget.Editor
	ctx    *tui.Context
	float  *widget.Float
}

func (s *scriptView) AcceptsFocus() bool { return false }

func (s *scriptView) Init(ctx *tui.Context) {
	s.Base.Init(ctx)
	s.ctx = ctx
	s.editor = widget.NewEditor(widget.WithEditorStyles(widget.TextInputStyles{
		Selection: cursorRowStyle,
	}))
	s.editor.SetValue(s.text)
	s.editor.SetReadOnly(true)
	ctx.Mount(s.editor)
	ctx.FocusComponent(s.editor)
}

func (s *scriptView) Layout(c tui.Constraints) tui.Size {
	// The float owns the size; the viewer fills it.
	w, h := c.MaxW, c.MaxH
	sz := s.ctx.LayoutChild(s.editor, tui.Tight(tui.Size{W: w, H: h}))
	s.ctx.PlaceChild(s.editor, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (s *scriptView) Render(tui.Surface) {}

// The read-only vim viewer forwards keys to the focused Editor, which leaves an
// unbound `q` to bubble in Normal mode (and, since golib v0.5.3, a no-op Esc too).
// So `q` and Esc both reach here and close the viewer — the same second dismiss
// key the other read-only floats carry.
func (s *scriptView) HandleEvent(ev tui.Event) bool {
	if dismissKey(ev) {
		s.float.Hide()
		return true
	}
	return false
}

func (s *scriptView) Add(...tui.Component) {}
func (s *scriptView) Remove(tui.Component) {}

// Move is a no-op — fixed shape, nothing to permute (see connPicker.Move).
func (s *scriptView) Move(tui.Component, int) {}
func (s *scriptView) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		if s.editor != nil {
			yield(s.editor)
		}
	}
}

func (s *scriptView) hints() []keyHint {
	return []keyHint{
		{"hjkl / w b", "move"}, {"v/V then y", "select and yank"},
		{"q/Esc", "close"},
	}
}

var _ tui.Container = (*scriptView)(nil)
