package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// resultsPanel renders the last execution (ADR-0057 §4): a Table whose
// cells the widget layer ellipsizes, a pretty-JSON toggle (Objective 16),
// an honest "more rows truncated" badge, and Enter/`v` value inspection in
// a float. The Table is rebuilt per result — column sets change per query.
type resultsPanel struct {
	widget.Base
	model *Model
	ctx   *tui.Context

	res      *ExecResult
	jsonMode bool

	table   tui.Component // *widget.Table[[]any] when a read result is shown
	rawList *widget.List[[]any]
	jsonV   *widget.BufferView
	text    *widget.Text // placeholder / write summaries
	current tui.Component
}

func newResultsPanel(m *Model) *resultsPanel {
	return &resultsPanel{
		model: m,
		text:  widget.NewText("no results — SPC r runs the query buffer"),
	}
}

func (p *resultsPanel) AcceptsFocus() bool { return true }

func (p *resultsPanel) Init(ctx *tui.Context) {
	p.Base.Init(ctx)
	p.ctx = ctx
	p.current = p.text
	ctx.Mount(p.current)
}

// swap replaces the mounted child.
func (p *resultsPanel) swap(c tui.Component) {
	if p.current == c {
		return
	}
	if p.current != nil {
		p.ctx.Unmount(p.current)
	}
	p.current = c
	p.ctx.Mount(c)
	p.RequestLayout()
	p.MarkDirty()
}

// Show installs a fresh result.
func (p *resultsPanel) Show(res *ExecResult) {
	p.res = res
	p.jsonMode = false
	p.rebuild()
}

// Clear drops the rendered result (instance change — nothing from the old
// server may keep rendering).
func (p *resultsPanel) Clear() {
	p.res = nil
	p.jsonMode = false
	p.text.SetText("no results — SPC r runs the query buffer")
	p.rebuild()
}

// ToggleJSON flips table/JSON rendering (leader j).
func (p *resultsPanel) ToggleJSON() {
	if p.res == nil || len(p.res.Columns) == 0 {
		return
	}
	p.jsonMode = !p.jsonMode
	p.rebuild()
}

func (p *resultsPanel) rebuild() {
	res := p.res
	switch {
	case res == nil:
		p.swap(p.text)
	case len(res.Columns) == 0:
		p.text.SetText(fmt.Sprintf("%s ok — %d row(s) affected in %s",
			strings.ToUpper(res.Verb), res.Affected, res.Duration))
		p.swap(p.text)
	case p.jsonMode:
		if p.jsonV == nil {
			p.jsonV = widget.NewBufferView()
		}
		// MOUNT FIRST: BufferView binds its writer handle in Init and
		// drops writes while unmounted (view.alive is false, the app
		// handle is nil) — writing before the swap silently produced an
		// empty JSON view.
		p.swap(p.jsonV)
		p.jsonV.Clear()
		out := make([]map[string]any, 0, len(res.Rows))
		for _, row := range res.Rows {
			m := make(map[string]any, len(res.Columns))
			for i, col := range res.Columns {
				if i < len(row) {
					m[col] = jsonCell(row[i])
				}
			}
			out = append(out, m)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			b = []byte(err.Error())
		}
		_, _ = p.jsonV.Writer().Write(append(b, '\n'))
		p.jsonV.ScrollTo(0) // a document, not a log tail
	default:
		cols := make([]widget.TableColumn[[]any], len(res.Columns))
		for i, name := range res.Columns {
			idx := i
			cols[i] = widget.TableColumn[[]any]{
				Title: name,
				Cell: func(row []any) string {
					if idx < len(row) {
						return renderCell(row[idx])
					}
					return ""
				},
			}
		}
		tbl := widget.NewTable(cols, widget.WithEmptyText[[]any]("0 rows"),
			widget.WithListStyles[[]any](widget.ListStyles{CursorRow: cursorRowStyle}))
		tbl.SetItems(res.Rows)
		p.table = tbl
		p.rawList = tbl.List()
		p.swap(tbl)
	}
}

func (p *resultsPanel) Layout(c tui.Constraints) tui.Size {
	sz := p.ctx.LayoutChild(p.current, c)
	p.ctx.PlaceChild(p.current, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(tui.Size{W: boundedMaxAvail(c.MaxW, sz.W), H: boundedMaxAvail(c.MaxH, sz.H)})
}

func boundedMaxAvail(bound, measured int) int {
	if bound == tui.Unbounded {
		return measured
	}
	return bound
}

func (p *resultsPanel) Render(s tui.Surface) {}

// StatusLine renders the result summary for the status bar.
func (p *resultsPanel) StatusLine() string {
	res := p.res
	if res == nil {
		return ""
	}
	if len(res.Columns) == 0 {
		return fmt.Sprintf("%s: %d affected (%s)", res.Verb, res.Affected, res.Duration)
	}
	line := fmt.Sprintf("%s: %d row(s) (%s)", res.Verb, len(res.Rows), res.Duration)
	if res.More {
		line += " — more rows truncated"
	}
	return line
}

func (p *resultsPanel) HandleEvent(ev tui.Event) bool {
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease {
		// Value inspection: v (or Enter via ActivateEvent below) opens the
		// selected row fully expanded.
		if k.Text == "v" && p.res != nil && p.rawList != nil && p.current == p.table {
			if idx, ok := p.rawList.Selected(); ok && idx < len(p.res.Rows) {
				p.model.openInspect(p.res.Columns, p.res.Rows[idx])
				return true
			}
		}
	}
	if p.current != nil {
		return p.current.HandleEvent(ev)
	}
	return false
}

// renderCell renders one wire value for a table cell (single line).
func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("0x%x", x)
	case string:
		// Cells are single-line; the widget layer ellipsizes width.
		return strings.ReplaceAll(strings.ReplaceAll(x, "\n", "␤"), "\r", "")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// jsonCell keeps JSON output faithful for bytes.
func jsonCell(v any) any {
	if b, ok := v.([]byte); ok {
		return fmt.Sprintf("0x%x", b)
	}
	return v
}

var _ = style.New // imported for future styling hooks
