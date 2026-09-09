package tui

import (
	"fmt"
	"iter"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Modal float plumbing: every dialog is a widget.Float on the
// Model's OverlayHost, removed from the stack on dismiss (the ddex-server
// recipe — floats must not accumulate as dead layers).

// openFloat shows content in a modal, titled float and returns it. The
// backdrop stays LIVE — no scrim: the float overlays the widgets rather
// than replacing them with gray space (Johno, M6 manual testing). The
// Box interior is filled, so the float itself remains opaque.
func (m *Model) openFloat(title string, content tui.Component) *widget.Float {
	return m.openFloatAt(title, content, widget.Center)
}

// modalSpan turns the space a float was offered into a modal dimension:
// a share of the terminal, floored so the modal stays usable on a small
// screen and capped so it stays readable on a wide one.
//
// Sizing lives HERE and in the bodies, never in the Box: a fixed width
// handed to style.Width is inert (golib reads propWidth nowhere in the
// layout path), which is why every modal used to render at its floor on
// any screen and the users footer wrapped at every width. The order of
// the clamps matters — the offered space wins last, so a body never
// returns more than its constraints allow.
func modalSpan(avail, pct, lo, hi int) int {
	if avail <= 0 {
		return 0
	}
	return min(min(max(avail*pct/100, lo), hi), avail)
}

// openFloatAt is openFloat with an explicit anchor (the `?` key card
// lives in the bottom-right corner).
// openFloatPct sizes the float as a share of the screen (golib
// WithSizeFraction) — for working surfaces that should follow a resize
// rather than sit at a fixed column count.
func (m *Model) openFloatPct(title string, content tui.Component, pct int) *widget.Float {
	return m.openFloatOpts(title, content, widget.Center,
		widget.WithSizeFraction(pct, pct))
}

func (m *Model) openFloatAt(title string, content tui.Component,
	anchor widget.Anchor) *widget.Float {
	return m.openFloatOpts(title, content, anchor)
}

func (m *Model) openFloatOpts(title string, content tui.Component,
	anchor widget.Anchor, extra ...widget.FloatOption) *widget.Float {
	// No fixed width: sizing is the CONTENT's job. style.Width sets
	// propWidth, which nothing in golib's layout path reads, so a column
	// count passed here was silently discarded — openForm asked for 56
	// and rendered 54, the help float asked for 64 and rendered 76.
	// Bodies size themselves with modalSpan and follow a resize.
	boxStyle := style.New().Border(style.BorderRounded)
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
	// The options the caller asked for. The INPUT is built by newForm rather
	// than here, because the advance hook has to close over the form and the
	// field's index, and neither exists yet at this call site.
	opts  []widget.TextInputOption
	input *widget.TextInput
}

func field(label string, opts ...widget.TextInputOption) formField {
	return formField{label: label, opts: opts}
}

// form is a column of labelled inputs, a hint footer and a status line.
//
// ENTER ADVANCES UNLESS THE LAST FIELD HOLDS FOCUS. It used to submit from any
// field, because widget.TextInput publishes SubmitEvent on Enter and this
// subscriber treated every one of them as a submit. So typing a token name and
// pressing Enter -- the obvious way to reach the next field -- submitted a
// half-filled form, and nothing on screen said which key advanced. The footer
// now says it, from the same vocabulary the `?` overlay uses.
//
// onSubmit returns the outcome: close the float, or show a status message and
// keep it open.
type form struct {
	tui  *tui.Context
	box  *widget.Box // set by the Model after openFloat for status updates
	flex *tui.Flex

	fields   []formField
	hint     *widget.Text
	status   *widget.Text
	onSubmit func(values []string) (close bool, status string)
	float    *widget.Float
}

func newForm(fields []formField, onSubmit func([]string) (bool, string)) *form {
	f := &form{
		fields: fields,
		// The footer, in the body rather than the Box: golib's Box offers a
		// title and no footer, and the managers already put their hints in
		// the body this way.
		hint:     widget.NewText(hintLine(formHints()), widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted)), widget.WithWrapMode(widget.Wrap)),
		status:   widget.NewText("", widget.WithTextStyle(style.New().Foreground(style.TokenError))),
		onSubmit: onSubmit,
	}
	f.flex = tui.NewFlex(tui.Vertical)
	for i := range f.fields {
		idx := i
		opts := make([]widget.TextInputOption, 0, len(f.fields[i].opts)+1)
		opts = append(opts, f.fields[i].opts...)
		// THE ADVANCE RUNS ON THE KEY, NOT ON AN EVENT. See advanceOrSubmit.
		opts = append(opts, widget.WithOnSubmit(func(string) { f.advanceOrSubmit(idx) }))
		f.fields[i].input = widget.NewTextInput(opts...)
		f.flex.Add(widget.NewText(f.fields[i].label,
			widget.WithTextStyle(style.New().Foreground(style.TokenTextMuted))))
		f.flex.Add(f.fields[i].input)
	}
	f.flex.Add(f.status)
	f.flex.Add(f.hint)
	return f
}

func (f *form) Init(ctx *tui.Context) {
	f.tui = ctx
	ctx.Mount(f.flex)
}

// advanceOrSubmit is Enter's whole behaviour, and it runs SYNCHRONOUSLY inside
// the key handler -- via widget.WithOnSubmit, not via widget.SubmitEvent.
//
// The event cannot do this. golib's Bus.Publish queues delivery onto the
// program lane, and the App selects between the input lane and the program
// lane, so when a keystroke is already waiting Go picks between them
// pseudo-randomly: the focus move landed before or after the next keystroke,
// about half the time each, and the keystroke that lost went to the field the
// operator had just left. Typing "demo" ENTER "sqlite" into the connection form
// produced a name of "demos" and an engine of "qlite", so the daemon refused an
// engine that does not exist. It passed locally, passed a full VM ledger, and
// failed in CI at the same commit. Every paste is that race, because a paste is
// a burst with none of a human's delay.
//
// THE LAST FIELD SUBMITS; every other one advances. Deciding by POSITION rather
// than by "is this the only field" keeps the single-field case correct for
// free: index 0 is also the last, so a one-field form (search, rename) submits
// on Enter exactly as it always did.
func (f *form) advanceOrSubmit(i int) {
	if i < 0 || i >= len(f.fields) {
		return
	}
	if i == len(f.fields)-1 {
		f.submit()
		return
	}
	// Focus could not move -- an unmounted or unfocusable field. Submitting
	// would be worse than doing nothing: it would fire a form the operator has
	// not finished, which is the very behaviour this replaced. Say so instead.
	if f.tui == nil || !f.tui.FocusComponent(f.fields[i+1].input) {
		f.status.SetText("could not move to the next field; use Tab")
	}
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
	cc := c
	// Text inputs are width-greedy, so a form is capped rather than
	// stretched (Johno, M6: login/new-workspace should stay compact) —
	// but it scales between the floor and the cap instead of sitting at
	// the floor on every screen.
	cc.MaxW = modalSpan(c.MaxW, formPct, formMinW, formMaxW)
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
	fm.float = m.openFloat(title, fm)
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
	// prose is shown ABOVE the keys, in the SAME float.
	//
	// A confirmation used to be two floats: the text in one, the actionable
	// menu stacked on top of it. A review found the consequence -- the
	// operator can press the key that acts while the thing being agreed to
	// sits behind the modal asking, and cancelling leaves the text floating
	// with nothing to act on it. For a security decision that is not a layout
	// preference. One surface: what you are agreeing to and the key that
	// agrees cannot be separated.
	prose []string
	float *widget.Float
}

func (l *leaderMenu) AcceptsFocus() bool { return true }

func (l *leaderMenu) Layout(c tui.Constraints) tui.Size {
	h := len(l.entries) + 1
	w := modalSpan(c.MaxW, leaderPct, leaderMinW, leaderMaxW)
	if n := len(l.prose); n > 0 {
		h += n + 1 // the prose, then a blank line before the keys
		// Wide enough for the text it carries: a consent line that wraps or
		// truncates is a consent line nobody read.
		for _, line := range l.prose {
			if len(line)+2 > w {
				w = min(c.MaxW, len(line)+2)
			}
		}
	}
	return c.Constrain(tui.Size{W: w, H: min(c.MaxH, h)})
}

func (l *leaderMenu) Render(s tui.Surface) {
	keySt := style.New().Foreground(style.TokenPrimary).Bold(true)
	muted := style.New().Foreground(style.TokenTextMuted)
	y := 0
	for _, line := range l.prose {
		if y >= s.Size().H {
			return
		}
		drawTo(s, 1, y, line, muted)
		y++
	}
	if len(l.prose) > 0 {
		y++ // the blank line
	}
	for _, e := range l.entries {
		if y >= s.Size().H {
			return
		}
		s.SetCell(1, y, string(e.key), keySt)
		drawTo(s, 4, y, e.label, style.New())
		y++
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
	lm.float = m.openFloat(title, lm)
}

// openLeaderWithProse is a confirmation whose TEXT AND KEYS ARE ONE FLOAT.
//
// Use it wherever the decision needs the reader to have seen something
// specific -- an exact set of addresses, a consequence they did not ask for.
// Two stacked floats let the key that acts sit on top of the thing being
// agreed to, which is how somebody confirms what they could not read.
func (m *Model) openLeaderWithProse(title, prose string, entries []leaderEntry) {
	lm := &leaderMenu{entries: entries, prose: strings.Split(strings.TrimRight(prose, "\n"), "\n")}
	lm.float = m.openFloat(title, lm)
}

// inspectFloat shows one result row as a navigable CELL list
// (value inspection is per cell): j/k select a cell, `y` imports the
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
	iv.float = m.openFloat("row — j/k: cell, y: copy to clipboard, Enter: full value, q/Esc: close", iv)
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
	iv.height = min(c.MaxH, min(len(iv.columns), modalSpan(c.MaxH, valueHPct, valueMinH, valueMaxH)))
	return c.Constrain(tui.Size{
		W: modalSpan(c.MaxW, valuePct, valueMinW, valueMaxW),
		H: max(iv.height, 1),
	})
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
		// Same copy contract as the value float: clipboard first, register
		// always, and never a claim the clipboard took it when it did not.
		text := faithfulCell(iv.cell(iv.cursor))
		iv.model.editor.SetRegister(text, false)
		msg, ok, dismiss := copyReport(iv.model.ctx.CopyToClipboard(text), false)
		if ok {
			iv.model.setOK(iv.columns[iv.cursor] + ": " + msg)
		} else {
			iv.model.setError(iv.columns[iv.cursor] + ": " + msg)
		}
		if dismiss {
			iv.float.Hide()
		}
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
	vf.float = m.openFloat(column+" — y: copy to clipboard, q/Esc: close", vf)
}

// openSecretFloat shows a credential that exists nowhere else. It is a
// valueFloat that refuses to dismiss itself on a FAILED copy: for a
// value the store cannot reproduce, closing on the fallback path would
// destroy it.
func (m *Model) openSecretFloat(title string, secret string) {
	vf := &valueFloat{model: m, view: widget.NewBufferView(), value: secret, secret: true}
	vf.float = m.openFloat(title, vf)
}

type valueFloat struct {
	widget.Base
	model *Model
	view  *widget.BufferView
	value any
	float *widget.Float
	ctx   *tui.Context
	// secret marks a value the store cannot reproduce (a freshly minted
	// PAT). It changes ONE thing: a copy that did not reach the system
	// clipboard leaves the float open instead of dismissing it.
	secret bool
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
	w := modalSpan(c.MaxW, valuePct, valueMinW, valueMaxW)
	h := modalSpan(c.MaxH, valueHPct, valueMinH, valueMaxH)
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
		// The editor register always gets it (in-app `p` keeps working);
		// the system clipboard is what the user actually needs when the
		// value is going into a psql string or a JDBC URL. OSC 52 is the
		// only mechanism that survives SSH and tmux, because the TERMINAL
		// performs the copy — but a backend may not support it, and
		// CopyToClipboard reports that rather than pretending.
		text := faithfulCell(vf.value)
		vf.model.editor.SetRegister(text, false)
		msg, ok, dismiss := copyReport(vf.ctx.CopyToClipboard(text), vf.secret)
		if ok {
			vf.model.setOK(msg)
		} else {
			vf.model.setError(msg)
		}
		if dismiss {
			vf.float.Hide()
		}
		return true
	}
	// Scrolling keys forward to the buffer view.
	return vf.view.HandleEvent(ev)
}

// copyReport turns a clipboard attempt into what the user is told and
// whether the float may dismiss itself.
//
// Two rules, both learned the hard way. A copy that did not reach the
// system clipboard is NEVER reported as one — the user acts on this line,
// and for a value the store cannot reproduce, acting on a false "copied"
// is unrecoverable. And on that failed path a secret float stays OPEN,
// because dismissing it is what destroys the credential.
func copyReport(reachedClipboard, secret bool) (msg string, ok, dismiss bool) {
	if reachedClipboard {
		// A SECRET float stays open on success too (Johno, v0.3.2 manual
		// testing). The value is shown once and cannot be recovered, so the
		// moment of dismissal belongs to the person who has to paste it —
		// not to autodb, which cannot know whether the paste landed. Closing
		// the instant the clipboard call returned took the credential away
		// while the user was still switching windows.
		return "copied to the system clipboard", true, !secret
	}
	return "clipboard unavailable — copied to the editor register only (p to paste)",
		false, !secret
}

// openTextFloat shows static text (help) in a scrollable float. It shares
// valueFloat's body, so it shares its sizing — the width argument it used
// to take was discarded twice over and is gone.
func (m *Model) openTextFloat(title, text string) {
	tf := &valueFloat{model: m, view: widget.NewBufferView(), value: text}
	tf.float = m.openFloat(title, tf)
}
