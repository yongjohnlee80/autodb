package tui

import (
	"iter"
	"strconv"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// connPicker chooses the connection the query editor runs against: a
// cursor over the available connections, Enter to pick — no typing a
// name (Johno, M6 manual testing).
type connPicker struct {
	widget.Base
	model *Model
	conns []ConnInfo
	list  *widget.List[ConnInfo]
	ctx   *tui.Context
	float *widget.Float
}

// The PICKER takes focus and drives its list — the same shape the
// manager floats use, so both behave identically. The list still paints
// its cursor row while unfocused, so the highlight is visible either way.
func (p *connPicker) AcceptsFocus() bool { return true }

// newConnPicker builds the picker with its list ready before mount, the
// same convention the forms use: content that exists at construction is
// visible to the float's focus walk and to Layout on the first frame.
func newConnPicker(m *Model, conns []ConnInfo) *connPicker {
	p := &connPicker{model: m, conns: conns}
	p.list = widget.NewList(
		widget.WithItems(conns, func(c ConnInfo) string {
			mark := "  "
			if c.ID == m.activeConn {
				mark = "▸ " // the current target
			}
			return mark + c.Name + "  (" + c.Engine + ", id " + strconv.FormatInt(c.ID, 10) + ")"
		}),
		widget.WithListStyles[ConnInfo](widget.ListStyles{CursorRow: cursorRowStyle}),
	)
	return p
}

func (p *connPicker) Init(ctx *tui.Context) {
	p.Base.Init(ctx)
	p.ctx = ctx
	ctx.Mount(p.list)
	// Start on the connection currently in use.
	for i, c := range p.conns {
		if c.ID == p.model.activeConn {
			p.list.SetCursor(i)
			break
		}
	}
}

func (p *connPicker) Layout(c tui.Constraints) tui.Size {
	w := min(c.MaxW, 58)
	h := min(c.MaxH, max(len(p.conns), 1))
	sz := p.ctx.LayoutChild(p.list, tui.Tight(tui.Size{W: w, H: h}))
	p.ctx.PlaceChild(p.list, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (p *connPicker) Render(tui.Surface) {}

func (p *connPicker) HandleEvent(ev tui.Event) bool {
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease {
		// Enter selects the highlighted row directly — no dependency on
		// the list's activation event reaching a subscriber.
		if k.Code == tui.KeyEnter {
			if i, ok := p.list.Selected(); ok && i < len(p.conns) {
				p.model.setActiveConn(p.conns[i])
			}
			p.float.Hide()
			return true
		}
	}
	return p.list.HandleEvent(ev)
}

// The picker is transparent to the framework's focus walk: without this
// the modal float cannot reach the list, so it never gets focus — no
// cursor, and Enter selects nothing.
func (p *connPicker) Add(...tui.Component) {}
func (p *connPicker) Remove(tui.Component) {}

// Move is a no-op: the picker is a fixed-shape container with a single
// child and no document order to permute. golib's tui.Container grew Move
// in v0.5.4 (golib tui ADR-0011 §2.2); satisfying it honestly here means
// doing nothing rather than pretending to reorder something.
func (p *connPicker) Move(tui.Component, int) {}
func (p *connPicker) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		if p.list != nil {
			yield(p.list)
		}
	}
}

var _ tui.Container = (*connPicker)(nil)

// hints powers the `?` card while the picker is open.
func (p *connPicker) hints() []keyHint {
	return []keyHint{
		{"j/k", "down / up"}, {"Enter", "use this connection"}, {"Esc", "cancel"},
	}
}
