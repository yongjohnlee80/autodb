package tui

import (
	"iter"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Modal float plumbing (ADR-0057 §9): every dialog is a widget.Float on the
// Model's OverlayHost, removed from the stack on dismiss (the ddex-server
// recipe — floats must not accumulate as dead layers).

// openFloat shows content in a modal, dimmed, titled float and returns it.
func (m *Model) openFloat(title string, content tui.Component, width int) *widget.Float {
	box := widget.NewBox(content,
		widget.WithTitle(title),
		widget.WithStyle(style.New().Width(width).Border(style.BorderRounded)),
		widget.WithFocusable(false),
	)
	f := widget.NewFloat(box, widget.WithModal(true), widget.WithDimBackground(true))
	m.host.Attach(f)
	f.Show()
	m.floats = append(m.floats, f)
	// Remove the layer once dismissed (Float hides itself on Esc; the
	// ddex-server recipe — layers must not accumulate).
	var unsub func()
	unsub = tui.SubscribeScoped(m.ctx, func(ev widget.DismissEvent) {
		if ev.Owner != f.NodeID() {
			return
		}
		m.host.Stack.Remove(f)
		for i, o := range m.floats {
			if o == f {
				m.floats = append(m.floats[:i], m.floats[i+1:]...)
				break
			}
		}
		if unsub != nil {
			unsub()
		}
	})
	return f
}

// modalOpen reports whether any float is still visible. Float.Hide flips
// Shown() synchronously while the DismissEvent that prunes m.floats is
// delivered a tick later — gating the leader on Shown() keeps a Space
// typed right behind an Esc from being swallowed.
func (m *Model) modalOpen() bool {
	for _, f := range m.floats {
		if f.Shown() {
			return true
		}
	}
	return false
}

// formField is one labelled text input.
type formField struct {
	label string
	input *widget.TextInput
}

func field(label string, opts ...widget.TextInputOption) formField {
	return formField{label: label, input: widget.NewTextInput(opts...)}
}

// form is a column of labelled inputs with a status line; Enter in any
// field submits. onSubmit returns the outcome: close the float, or show a
// status message and keep it open.
type form struct {
	tui  *tui.Context
	box  *widget.Box // set by the Model after openFloat for status updates
	flex *tui.Flex

	fields   []formField
	status   *widget.Text
	onSubmit func(values []string) (close bool, status string)
	float    *widget.Float
}

func newForm(fields []formField, onSubmit func([]string) (bool, string)) *form {
	f := &form{
		fields:   fields,
		status:   widget.NewText("", widget.WithTextStyle(style.New().Foreground(style.TokenError))),
		onSubmit: onSubmit,
	}
	f.flex = tui.NewFlex(tui.Vertical)
	for _, fd := range fields {
		f.flex.Add(widget.NewText(fd.label, widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted))))
		f.flex.Add(fd.input)
	}
	f.flex.Add(f.status)
	return f
}

func (f *form) Init(ctx *tui.Context) {
	f.tui = ctx
	ctx.Mount(f.flex)
	tui.SubscribeScoped(ctx, func(ev widget.SubmitEvent) {
		for _, fd := range f.fields {
			if ev.Owner == fd.input.NodeID() {
				f.submit()
				return
			}
		}
	})
}

func (f *form) submit() {
	values := make([]string, len(f.fields))
	for i, fd := range f.fields {
		values[i] = fd.input.Value()
	}
	closeIt, status := f.onSubmit(values)
	if closeIt {
		if f.float != nil {
			f.float.Hide()
		}
		return
	}
	f.status.SetText(status)
}

func (f *form) Layout(c tui.Constraints) tui.Size {
	sz := f.tui.LayoutChild(f.flex, c)
	f.tui.PlaceChild(f.flex, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(sz)
}

func (f *form) Render(tui.Surface) {}

func (f *form) HandleEvent(ev tui.Event) bool { return false }

// form is transparent to the framework's focus walk (tui.Container):
// without this, the modal Float's focus seeding cannot reach the text
// inputs and the first keystrokes land in the wrong field.
func (f *form) Add(...tui.Component) {}
func (f *form) Remove(tui.Component) {}
func (f *form) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		yield(f.flex)
	}
}

var _ tui.Container = (*form)(nil)

// openForm builds a form float and wires the float back-reference.
func (m *Model) openForm(title string, fields []formField, onSubmit func([]string) (bool, string)) *form {
	fm := newForm(fields, onSubmit)
	fm.float = m.openFloat(title, fm, 56)
	return fm
}

// leaderEntry is one binding in the Space menu.
type leaderEntry struct {
	key   rune
	label string
	run   func()
}

// leaderMenu is the which-key float: one more keypress executes and
// dismisses; Esc cancels (the Float's own modal Esc handling).
type leaderMenu struct {
	widget.Base
	entries []leaderEntry
	float   *widget.Float
}

func (l *leaderMenu) AcceptsFocus() bool { return true }

func (l *leaderMenu) Layout(c tui.Constraints) tui.Size {
	return c.Constrain(tui.Size{W: min(c.MaxW, 46), H: min(c.MaxH, len(l.entries)+1)})
}

func (l *leaderMenu) Render(s tui.Surface) {
	keySt := style.New().Foreground(style.TokenPrimary).Bold(true)
	for i, e := range l.entries {
		if i >= s.Size().H {
			break
		}
		s.SetCell(1, i, string(e.key), keySt)
		drawTo(s, 4, i, e.label, style.New())
	}
}

func (l *leaderMenu) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease || k.Text == "" {
		return false
	}
	r := []rune(k.Text)[0]
	for _, e := range l.entries {
		if e.key == r {
			l.float.Hide()
			e.run()
			return true
		}
	}
	return false
}

// drawTo paints s truncated into the row (local helper — widget internals
// like drawText are unexported).
func drawTo(s tui.Surface, x, y int, text string, st style.Style) {
	for _, r := range text {
		w := s.StringWidth(string(r))
		if x+w > s.Size().W {
			return
		}
		s.SetCell(x, y, string(r), st)
		x += w
	}
}

// openLeader shows the leader menu.
func (m *Model) openLeader(entries []leaderEntry) {
	lm := &leaderMenu{entries: entries}
	lm.float = m.openFloat("SPC — commands", lm, 48)
}

// inspectFloat shows one result row with every cell fully expanded; `y`
// imports the rendered text into the editor's register.
type inspectFloat struct {
	widget.Base
	view  *widget.BufferView
	text  string
	model *Model
	float *widget.Float
	ctx   *tui.Context
}

func (m *Model) openInspect(columns []string, row []any) {
	var sb strings.Builder
	for i, col := range columns {
		val := "NULL"
		if i < len(row) && row[i] != nil {
			val = renderCell(row[i])
		}
		sb.WriteString(col)
		sb.WriteString(" = ")
		sb.WriteString(val)
		sb.WriteString("\n")
	}
	iv := &inspectFloat{view: widget.NewBufferView(), text: sb.String(), model: m}
	iv.float = m.openFloat("value — y: copy to editor register, Esc: close", iv, 76)
}

func (iv *inspectFloat) AcceptsFocus() bool { return true }

func (iv *inspectFloat) Init(ctx *tui.Context) {
	iv.Base.Init(ctx)
	iv.ctx = ctx
	ctx.Mount(iv.view)
	_, _ = iv.view.Writer().Write([]byte(iv.text))
}

func (iv *inspectFloat) Layout(c tui.Constraints) tui.Size {
	w := min(c.MaxW, 74)
	h := min(c.MaxH, 18)
	sz := iv.ctx.LayoutChild(iv.view, tui.Tight(tui.Size{W: w, H: h}))
	iv.ctx.PlaceChild(iv.view, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (iv *inspectFloat) Render(tui.Surface) {}

func (iv *inspectFloat) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	if k.Text == "y" {
		iv.model.editor.SetRegister(iv.text, false)
		iv.model.setStatus("value copied to editor register (p to paste)")
		iv.float.Hide()
		return true
	}
	// Scrolling keys forward to the buffer view.
	return iv.view.HandleEvent(ev)
}
