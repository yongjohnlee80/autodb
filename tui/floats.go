package tui

import (
	"fmt"
	"iter"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Modal float plumbing (ADR-0057 §9): every dialog is a widget.Float on the
// Model's OverlayHost, removed from the stack on dismiss (the ddex-server
// recipe — floats must not accumulate as dead layers).

// openFloat shows content in a modal, titled float and returns it. The
// backdrop stays LIVE — no scrim: the float overlays the widgets rather
// than replacing them with gray space (Johno, M6 manual testing). The
// Box interior is filled, so the float itself remains opaque.
func (m *Model) openFloat(title string, content tui.Component, width int) *widget.Float {
	return m.openFloatAt(title, content, width, widget.Center)
}

// openFloatAt is openFloat with an explicit anchor (the `?` key card
// lives in the bottom-right corner).
// openFloatPct sizes the float as a share of the screen (golib
// WithSizeFraction) — for working surfaces that should follow a resize
// rather than sit at a fixed column count.
func (m *Model) openFloatPct(title string, content tui.Component, pct int) *widget.Float {
	return m.openFloatOpts(title, content, 0, widget.Center,
		widget.WithSizeFraction(pct, pct))
}

func (m *Model) openFloatAt(title string, content tui.Component, width int,
	anchor widget.Anchor) *widget.Float {
	return m.openFloatOpts(title, content, width, anchor)
}

func (m *Model) openFloatOpts(title string, content tui.Component, width int,
	anchor widget.Anchor, extra ...widget.FloatOption) *widget.Float {
	boxStyle := style.New().Border(style.BorderRounded)
	if width > 0 {
		boxStyle = boxStyle.Width(width)
	}
	// width <= 0 hands sizing to the CONTENT, which is how a body sizes
	// itself as a fraction of the screen (the history and script views):
	// a fixed column count cannot follow a resized terminal.
	box := widget.NewBox(content,
		widget.WithTitle(title),
		widget.WithStyle(boxStyle),
		widget.WithFocusable(false),
	)
	opts := append([]widget.FloatOption{
		widget.WithModal(true), widget.WithAnchor(anchor),
	}, extra...)
	f := widget.NewFloat(box, opts...)
	m.host.Attach(f)
	f.Show()
	m.floats = append(m.floats, openFloatRef{f: f, body: content, title: title})
	// Remove the layer once dismissed (Float hides itself on Esc; the
	// ddex-server recipe — layers must not accumulate).
	var unsub func()
	unsub = tui.SubscribeScoped(m.ctx, func(ev widget.DismissEvent) {
		if ev.Owner != f.NodeID() {
			return
		}
		m.host.Stack.Remove(f)
		for i, o := range m.floats {
			if o.f == f {
				m.floats = append(m.floats[:i], m.floats[i+1:]...)
				break
			}
		}
		if unsub != nil {
			unsub()
		}
		// A login prompt suppressed while this float was up fires now —
		// the CodeAuth transition is retained, never dropped.
		m.maybePromptLogin()
	})
	return f
}

// modalOpen reports whether any float is still visible. Float.Hide flips
// Shown() synchronously while the DismissEvent that prunes m.floats is
// delivered a tick later — gating the leader on Shown() keeps a Space
// typed right behind an Esc from being swallowed.
func (m *Model) modalOpen() bool {
	for _, f := range m.floats {
		if f.f.Shown() {
			return true
		}
	}
	return false
}

// dismissKey reports whether ev is a bare `q` — the second dismiss key,
// alongside the framework's Esc, for the READ-ONLY and NAVIGATIONAL modals:
// the row/value inspectors, the connection picker, the managers, the script
// history and its viewer, the about card. Those surfaces only look and move, so
// `q` closing them matches the app's vim identity and the global `q`-quit.
//
// It is deliberately NOT wired into the text-entry forms, where `q` is a typed
// character: dismissing there would make it impossible to type `q` into a CIDR,
// a note name, a password, or a PAT label. That exclusion is permanent.
//
// The single-key surfaces — leaderMenu and the confirmations built on it — DO
// honour it as of the quit-confirmation change, but only as a fallback after
// their own bindings: see leaderMenu.HandleEvent. `q` there means "close this"
// exactly when the menu has nothing else to say about it, which is why the SPC
// menu's `q` (focus query editor) is unaffected.
//
// golib's Editor leaves an unbound `q` to bubble in Normal mode, so even the
// read-only vim viewer can use it.
func dismissKey(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	return ok && k.Kind != tui.KeyRelease && k.Text == "q"
}

// openFloatRef remembers what a float is showing, so `?` can report the
// keys of whatever currently owns the screen.
type openFloatRef struct {
	f     *widget.Float
	body  tui.Component
	title string
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

// formWidth caps the form body: text inputs are width-greedy, and an
// uncapped form stretches its float across the whole screen (Johno, M6
// manual testing — login/new-workspace should be compact modals). The
// managers cap themselves the same way.
const formWidth = 52

func (f *form) Layout(c tui.Constraints) tui.Size {
	cc := c
	cc.MaxW = min(c.MaxW, formWidth)
	sz := f.tui.LayoutChild(f.flex, cc)
	f.tui.PlaceChild(f.flex, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return cc.Constrain(sz)
}

func (f *form) Render(tui.Surface) {}

func (f *form) HandleEvent(ev tui.Event) bool { return false }

// form is transparent to the framework's focus walk (tui.Container):
// without this, the modal Float's focus seeding cannot reach the text
// inputs and the first keystrokes land in the wrong field.
func (f *form) Add(...tui.Component) {}
func (f *form) Remove(tui.Component) {}

// Move is a no-op — fixed shape, nothing to permute (see connPicker.Move).
func (f *form) Move(tui.Component, int) {}
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

// leaderResolve decides what a key means inside a leader menu.
//
// A BOUND entry always wins; `q` dismisses only when nothing binds it. The
// order is the whole design: the SPC menu binds `q` to "focus query editor",
// so a dismiss-first rule would silently delete a real command. Resolving
// bindings first means every menu and confirmation that does NOT bind `q`
// gains Esc's behaviour, and the one that does keeps working.
//
// It returns the index of the entry to run, or -1; dismiss reports the
// fallback. Separated from HandleEvent because this precedence is the part
// worth testing, and a Float is not needed to state it.
func leaderResolve(entries []leaderEntry, ev tui.Event) (idx int, dismiss bool) {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease || k.Text == "" {
		return -1, false
	}
	r := []rune(k.Text)[0]
	for i, e := range entries {
		if e.key == r {
			return i, false
		}
	}
	return -1, dismissKey(ev)
}

func (l *leaderMenu) HandleEvent(ev tui.Event) bool {
	idx, dismiss := leaderResolve(l.entries, ev)
	switch {
	case idx >= 0:
		l.float.Hide()
		l.entries[idx].run()
		return true
	case dismiss:
		l.float.Hide()
		return true
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

// openLeader shows a which-key style chooser. The TITLE matters: these
// floats are also used for confirmations and conflict choices, and
// titling every one of them "SPC — commands" told the user nothing
// (and made two very different prompts indistinguishable).
func (m *Model) openLeader(title string, entries []leaderEntry) {
	lm := &leaderMenu{entries: entries}
	lm.float = m.openFloat(title, lm, 48)
}

// inspectFloat shows one result row as a navigable CELL list (ADR-0057
// §4 — value inspection is per cell): j/k select a cell, `y` imports the
// selected cell's FAITHFUL value into the editor's register, Enter opens
// the full value in a scrollable float.
type inspectFloat struct {
	widget.Base
	model   *Model
	float   *widget.Float
	columns []string
	row     []any
	cursor  int
	top     int
	height  int
}

func (m *Model) openInspect(columns []string, row []any) {
	iv := &inspectFloat{model: m, columns: columns, row: row}
	iv.float = m.openFloat("row — j/k: cell, y: copy value, Enter: full value, q/Esc: close", iv, 76)
}

// faithfulCell renders a cell's value VERBATIM for the register: real
// newlines, no display substitutions (renderCell is the display form).
func faithfulCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return bytesText(x)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

func (iv *inspectFloat) cell(i int) any {
	if i < len(iv.row) {
		return iv.row[i]
	}
	return nil
}

func (iv *inspectFloat) AcceptsFocus() bool { return true }

func (iv *inspectFloat) Layout(c tui.Constraints) tui.Size {
	iv.height = min(c.MaxH, min(len(iv.columns), 18))
	return c.Constrain(tui.Size{W: min(c.MaxW, 74), H: max(iv.height, 1)})
}

func (iv *inspectFloat) Render(s tui.Surface) {
	if iv.cursor < iv.top {
		iv.top = iv.cursor
	}
	if iv.cursor >= iv.top+iv.height {
		iv.top = iv.cursor - iv.height + 1
	}
	selSt := cursorRowStyle
	for line := 0; line < iv.height; line++ {
		i := iv.top + line
		if i >= len(iv.columns) {
			break
		}
		st := style.New()
		marker := "  "
		if i == iv.cursor {
			st = selSt
			marker = "❯ "
		}
		drawTo(s, 0, line, marker+iv.columns[i]+" = "+renderCell(iv.cell(i)), st)
	}
}

func (iv *inspectFloat) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	if dismissKey(ev) {
		iv.float.Hide()
		return true
	}
	switch {
	case k.Text == "j", k.Code == tui.KeyDown:
		if iv.cursor < len(iv.columns)-1 {
			iv.cursor++
			iv.MarkDirty()
		}
		return true
	case k.Text == "k", k.Code == tui.KeyUp:
		if iv.cursor > 0 {
			iv.cursor--
			iv.MarkDirty()
		}
		return true
	case k.Text == "y":
		iv.model.editor.SetRegister(faithfulCell(iv.cell(iv.cursor)), false)
		iv.model.setStatus(iv.columns[iv.cursor] + " copied to editor register (p to paste)")
		iv.float.Hide()
		return true
	case k.Code == tui.KeyEnter:
		col := iv.columns[iv.cursor]
		iv.model.openValueFloat(col, iv.cell(iv.cursor))
		return true
	}
	return false
}

// openValueFloat shows ONE cell's full value in a scrollable view; `y`
// imports the faithful value into the editor's register.
func (m *Model) openValueFloat(column string, val any) {
	vf := &valueFloat{model: m, view: widget.NewBufferView(), value: val}
	vf.float = m.openFloat(column+" — y: copy to editor register, q/Esc: close", vf, 76)
}

type valueFloat struct {
	widget.Base
	model *Model
	view  *widget.BufferView
	value any
	float *widget.Float
	ctx   *tui.Context
}

func (vf *valueFloat) AcceptsFocus() bool { return true }

func (vf *valueFloat) Init(ctx *tui.Context) {
	vf.Base.Init(ctx)
	vf.ctx = ctx
	ctx.Mount(vf.view)
	_, _ = vf.view.Writer().Write([]byte(faithfulCell(vf.value) + "\n"))
	vf.view.ScrollTo(0) // static content reads top-down, not log-tailed
}

func (vf *valueFloat) Layout(c tui.Constraints) tui.Size {
	w := min(c.MaxW, 74)
	h := min(c.MaxH, 18)
	sz := vf.ctx.LayoutChild(vf.view, tui.Tight(tui.Size{W: w, H: h}))
	vf.ctx.PlaceChild(vf.view, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return c.Constrain(tui.Size{W: w, H: h})
}

func (vf *valueFloat) Render(tui.Surface) {}

func (vf *valueFloat) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	if dismissKey(ev) {
		vf.float.Hide()
		return true
	}
	if k.Text == "y" {
		vf.model.editor.SetRegister(faithfulCell(vf.value), false)
		vf.model.setStatus("value copied to editor register (p to paste)")
		vf.float.Hide()
		return true
	}
	// Scrolling keys forward to the buffer view.
	return vf.view.HandleEvent(ev)
}

// openTextFloat shows static text (help) in a scrollable float.
func (m *Model) openTextFloat(title, text string, width int) {
	tf := &valueFloat{model: m, view: widget.NewBufferView(), value: text}
	tf.float = m.openFloat(title, tf, width)
}
