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
	// A FLOAT TAKES THE FOCUSED FRAME UNCONDITIONALLY. It is declared
	// non-focusable -- focus belongs to the controls inside it -- so the Box's
	// own focused/unfocused switch never fires; and a modal float is by
	// definition the surface with the attention, so the faint frame would be
	// wrong every time it was drawn. See panelStyles.
	_, panelFocused := panelStyles()
	boxStyle := panelFocused.Border(style.BorderRounded)
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
	// Remove the layer once dismissed (Float hides itself on Esc; the
	// ddex-server recipe — layers must not accumulate).
	m.trackOverlay(f, content, title, func() { m.host.Stack.Remove(f) })
	return f
}

// modalOpen reports whether any float is still visible. Float.Hide flips
// Shown() synchronously while the DismissEvent that prunes m.floats is
// delivered a tick later — gating the leader on Shown() keeps a Space
// typed right behind an Esc from being swallowed.
func (m *Model) modalOpen() bool {
	for _, f := range m.floats {
		if f.o.Shown() {
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
// The single-key surfaces — leaderMenu and the menus built on it — DO honour it,
// but only as a fallback after their own bindings: see leaderMenu.HandleEvent.
// `q` there means "close this" exactly when the menu has nothing else to say
// about it, which is why the SPC menu's `q` (focus query editor) is unaffected.
//
// THE CONFIRMATIONS NO LONGER COME THROUGH HERE. They are widget.Modal now, and
// they declare `q` to golib directly via WithModalDismissKeys — see
// openDialogOpts. The rule is the same one this comment has always described;
// only the mechanism moved. The forms declare nothing, which is this comment's
// permanent exclusion, unchanged.
//
// golib's Editor leaves an unbound `q` to bubble in Normal mode, so even the
// read-only vim viewer can use it.
func dismissKey(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	return ok && k.Kind != tui.KeyRelease && k.Text == "q"
}

// overlay is the part of an open surface the Model has to be able to drive:
// whether it is up, how to take it down, and which NodeID a DismissEvent will
// name.
//
// It is an INTERFACE because a Modal is not a Float. Everything the Model
// tracks today is a widget.Float, which satisfies this as it stands; the forms
// become widget.Modal, which spells the same three operations differently and
// arrives through an adapter. Without the interface the registry would simply
// stop seeing the newest surfaces — and nothing would fail, which is the worst
// shape a regression can take: a reconnect would leave a dialog on screen
// showing data from a server that is gone, a deferred login prompt would open
// on top of one, and `?` would report the panel behind it.
type overlay interface {
	Shown() bool
	Hide()
	NodeID() tui.NodeID
}

// modalOverlay adapts widget.Modal, which spells the same three operations
// differently: IsOpen for Shown, and Dismiss — which needs a REASON a bare
// Hide() has nowhere to put. Programmatic is the right one here, because every
// caller of Hide is the program closing a surface (a reconnect, a submit), not
// the operator pressing anything.
type modalOverlay struct{ m *widget.Modal }

func (o modalOverlay) Shown() bool        { return o.m.IsOpen() }
func (o modalOverlay) Hide()              { o.m.Dismiss(widget.DismissProgrammatic) }
func (o modalOverlay) NodeID() tui.NodeID { return o.m.NodeID() }

// trackOverlay registers an open surface and arranges its removal, for floats
// and dialogs alike.
//
// TWO EVENTS, ONE PRUNE. A Float announces its closing with DismissEvent and a
// Modal with OverlayDismissedEvent — different types carrying the same owner
// NodeID. Subscribing to one of them would leave the other kind of surface in
// the registry forever: the layer would stay listed after it closed, a deferred
// login would wait behind a dialog that is no longer there, and `?` would
// report a surface the operator had already dismissed. None of that fails
// loudly, which is exactly why it has to be handled here rather than noticed
// later.
//
// The prune is idempotent and owner-addressed, because these are not mutually
// exclusive: a future surface that publishes both must be removed once.
func (m *Model) trackOverlay(o overlay, body tui.Component, title string, detach func()) {
	m.floats = append(m.floats, openOverlayRef{o: o, body: body, title: title})
	var unsubs []func()
	pruned := false
	prune := func(owner tui.NodeID) {
		if pruned || owner != o.NodeID() {
			return
		}
		pruned = true
		if detach != nil {
			detach()
		}
		for i, e := range m.floats {
			if e.o == o {
				m.floats = append(m.floats[:i], m.floats[i+1:]...)
				break
			}
		}
		for _, u := range unsubs {
			if u != nil {
				u()
			}
		}
		// A login prompt suppressed while this surface was up fires now —
		// the CodeAuth transition is retained, never dropped.
		m.maybePromptLogin()
	}
	unsubs = append(unsubs,
		tui.SubscribeScoped(m.ctx, func(ev widget.DismissEvent) { prune(ev.Owner) }),
		tui.SubscribeScoped(m.ctx, func(ev widget.OverlayDismissedEvent) { prune(ev.Owner) }),
	)
}

// openOverlayRef remembers what an open surface is showing, so `?` can report
// the keys of whatever currently owns the screen. Ordered: the slice is
// append-only and the LAST shown entry is the topmost.
type openOverlayRef struct {
	o     overlay
	body  tui.Component
	title string
}

// formControl is one form row's widget and the answer it yields.
//
// IT YIELDS A TYPED VALUE, and that is the point of the interface. Every field
// used to be a *widget.TextInput read back as a string, so a connection id went
// out as text and came back through strconv — the stringly-typed round trip
// that put "type a connection id" in the product to begin with. A select yields
// the id it was GIVEN: the program keeps the number, the operator reads the
// name.
type formControl interface {
	// component is the focusable widget the form lays out.
	component() tui.Component
	// value is the typed answer: a string from a text input, an int64 from an
	// id select, whatever a select was parameterised with.
	value() any
}

// textControl is a free-text row. Its answer is a string, untrimmed — trimming
// is the caller's decision, because a passphrase is not trimmed and a name is.
type textControl struct{ in *widget.TextInput }

func (t textControl) component() tui.Component { return t.in }
func (t textControl) value() any               { return t.in.Value() }

// formField is one labelled row.
type formField struct {
	label string
	// build makes the row's control, and it is a CLOSURE rather than a kind
	// tag because a select's option type varies by field — int64 for ids,
	// string for the enums — and a heterogeneous []formField cannot carry a
	// type parameter. The closure captures it instead. It receives the form and
	// its own index because a live select has to know, when its options finally
	// arrive, whether the form they were asked for is still open.
	build func(f *form, i int) formControl
	ctl   formControl
}

func field(label string, opts ...widget.TextInputOption) formField {
	return formField{label: label, build: func(f *form, i int) formControl {
		o := make([]widget.TextInputOption, 0, len(opts)+2)
		// THE SHARED LOOK GOES ON FIRST so a caller's own styles, passed in
		// opts, still win: WithTextInputStyles inherits from what is already
		// set, and last writer decides.
		o = append(o, widget.WithTextInputStyles(inputValueStyles()))
		o = append(o, opts...)
		// THE ADVANCE RUNS ON THE KEY, NOT ON AN EVENT. See advanceOrSubmit.
		o = append(o, widget.WithOnSubmit(func(string) { f.advanceOrSubmit(i) }))
		return textControl{in: widget.NewTextInput(o...)}
	}}
}

// formValues are a submitted form's answers, read by position and BY TYPE.
//
// The accessors are deliberately narrow. `str` trims, because every name, CIDR
// and label in this package trims; `raw` does not, because a passphrase must
// not; and `id` reports whether an id is actually present rather than handing
// back a zero that reads like a real row.
type formValues []any

func (v formValues) has(i int) bool { return i >= 0 && i < len(v) }

func (v formValues) raw(i int) string {
	if !v.has(i) {
		return ""
	}
	s, _ := v[i].(string)
	return s
}

func (v formValues) str(i int) string { return strings.TrimSpace(v.raw(i)) }

// id reports the row identity a select yielded. ok is false when the field is
// absent, is not an id field, or holds no choice — a caller must not read 0 as
// "the first row".
func (v formValues) id(i int) (n int64, ok bool) {
	if !v.has(i) {
		return 0, false
	}
	n, ok = v[i].(int64)
	return n, ok && n > 0
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
	labels   []*widget.Text
	hint     *widget.Text
	status   *widget.Text
	onSubmit func(values formValues) (close bool, status string)
	// surface is the dialog this form is the body of. A form closes itself
	// through the overlay interface rather than a concrete widget, so the one
	// submit path serves however the surface is presented.
	surface overlay
	// ok is the affirmative button. Enter on the last field moves focus HERE
	// rather than submitting: no field submits, which is what frees Enter for
	// a select to open its options.
	ok *widget.Button
	// submitted records that the form closed ITSELF, having accepted the
	// answers. Dismissal is otherwise indistinguishable from a cancel: a
	// successful submit hides the surface, and hiding it publishes the same
	// event Escape does. Without this the cancel callback would run on every
	// successful submit as well.
	submitted bool
}

// values is the answers as the controls currently hold them.
func (f *form) values() formValues {
	v := make(formValues, len(f.fields))
	for i, fd := range f.fields {
		if fd.ctl == nil {
			continue
		}
		v[i] = fd.ctl.value()
	}
	return v
}

// prose is rendered ABOVE the fields and is NOT a field. Keeping it out of
// f.fields is what stops a line of text entering the focus traversal, where
// Tab would stop on something nobody can type into and the operator would be
// left pressing keys at a paragraph.
func newForm(fields []formField, onSubmit func(formValues) (bool, string), prose []string) *form {
	f := &form{
		fields: fields,
		// The footer, in the body rather than the Box: golib's Box offers a
		// title and no footer, and the managers already put their hints in
		// the body this way.
		hint:     widget.NewText(hintLine(formHints()), widget.WithTextStyle(mutedStyle()), widget.WithWrapMode(widget.Wrap)),
		status:   widget.NewText("", widget.WithTextStyle(style.New().Foreground(style.TokenError))),
		onSubmit: onSubmit,
	}
	f.flex = tui.NewFlex(tui.Vertical)
	for _, line := range prose {
		f.flex.Add(widget.NewText(line, widget.WithWrapMode(widget.Wrap)))
	}
	f.labels = make([]*widget.Text, len(f.fields))
	for i := range f.fields {
		f.fields[i].ctl = f.fields[i].build(f, i)
		f.labels[i] = widget.NewText(f.fields[i].label,
			widget.WithTextStyle(inputLabelStyle()))
		f.flex.Add(f.labels[i])
		f.flex.Add(f.fields[i].ctl.component())
	}
	// THE FOOTER BAND. Everything below the rule is chrome: the status line
	// that reports a refusal, and the key hints. Above it is the operator's
	// data. The buttons golib draws after the body then sit below a line
	// rather than below a sentence, which is what the hints used to look
	// like -- one more text row of the modal's own content.
	f.flex.Add(newHRule())
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
// NO FIELD SUBMITS. Enter advances, and from the LAST field it moves to the OK
// button rather than firing the form. The positional rule -- "the last field
// submits" -- is deleted rather than worked around: it is the thing a select
// could not live under, because Enter on a select opens its options and is
// therefore not available to mean "submit" as well. With a button there is
// nothing left for position to decide, and a one-field form behaves like every
// other one instead of being a special case that happens to work.
func (f *form) advanceOrSubmit(i int) {
	if i < 0 || i >= len(f.fields) {
		return
	}
	if i == len(f.fields)-1 {
		if f.tui != nil && f.ok != nil && f.tui.FocusComponent(f.ok) {
			return
		}
		f.status.SetText("use Tab to reach OK")
		return
	}
	// Focus could not move -- an unmounted or unfocusable field. Submitting
	// would be worse than doing nothing: it would fire a form the operator has
	// not finished, which is the very behaviour this replaced. Say so instead.
	if f.tui == nil || !f.tui.FocusComponent(f.fields[i+1].ctl.component()) {
		f.status.SetText("could not move to the next field; use Tab")
	}
}

// open reports whether the form is still on screen. A live select's load
// checks this before applying anything: options that arrive after the operator
// has walked away belong to nobody.
func (f *form) open() bool { return f.surface != nil && f.surface.Shown() }

// refreshLabels re-renders each row's label with whatever its control has to
// say about itself — loading, empty, or the error that stopped it.
func (f *form) refreshLabels() {
	for i := range f.fields {
		if i >= len(f.labels) || f.labels[i] == nil {
			continue
		}
		text := f.fields[i].label
		if a, ok := f.fields[i].ctl.(labelAnnotator); ok {
			text += a.annotation()
		}
		f.labels[i].SetText(text)
	}
}

func (f *form) submit() {
	// A control may refuse. A select whose options have not arrived, or whose
	// load failed, cannot contribute an answer the operator actually chose —
	// and submitting anyway would send a choice made from a list that was
	// never fully shown.
	for i := range f.fields {
		g, ok := f.fields[i].ctl.(submitGate)
		if !ok {
			continue
		}
		if blocked, why := g.blocks(); blocked {
			f.status.SetText(f.fields[i].label + ": " + why)
			return
		}
	}
	closeIt, status := f.onSubmit(f.values())
	if closeIt {
		// SET BEFORE THE HIDE, not after: Hide publishes the dismissal
		// synchronously, and the cancel hook reads this flag from inside it.
		f.submitted = true
		if f.surface != nil {
			f.surface.Hide()
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
// openForm shows a form as a DIALOG: a titled card with the fields, and a
// button row that is the only route to a submit.
//
// The button row is not decoration. Enter used to submit from the last field, a
// rule decided by POSITION, and a select cannot live under it — Enter on a
// select opens its options, so the key is already spoken for and the field can
// neither advance nor submit with it. Advancing on the select's change event
// instead would reintroduce the lane race that advanceOrSubmit exists to avoid.
// A button removes the question rather than answering it.
// formOpts are the modal's presentation choices.
//
// A STRUCT RATHER THAN MORE POSITIONAL ARGUMENTS. openFormOpts already took a
// bare `scrim bool` that read as nothing at the call site, and the factory
// needs four more. Named fields mean a reader sees which knob is being turned
// without counting commas, and a new one does not touch every caller.
type formOpts struct {
	// scrim fades the backdrop. Default false: the operator is usually reading
	// the thing behind the dialog.
	scrim bool
	// prose is shown ABOVE the fields, and is NOT a field. Keeping it out of
	// the field list is what stops a non-focusable row entering the focus
	// traversal, where Tab would stop on text nobody can type into.
	prose []string
	// okText renames the affirmative button.
	okText string
	// noCancel leaves only the affirmative button. For acknowledgements.
	noCancel bool
	// onCancel runs when the operator declines, and receives the answers as
	// they stood — no binding has been written.
	//
	// IT IS WIRED TO THE DISMISSAL, NOT TO THE BUTTON, and the difference is
	// narrow but real. Escape alone would not need it: widget.Modal resolves a
	// dismiss request by activating the Cancel-ROLE button, so a callback
	// hanging off that button already runs for Escape and for a host dismiss
	// key.
	//
	// What it does not run for is a dialog with NO cancel button. An
	// acknowledgement built with noCancel has no Cancel role for Modal to
	// resolve against, so Escape closes it through the other arm of that
	// branch and the button callback -- there being no button -- never runs.
	// Hanging the callback off the dismissal instead covers that case and the
	// ones the button covers, with one wire rather than two.
	onCancel func(formValues)
}

func (m *Model) openForm(title string, fields []formField, onSubmit func(formValues) (bool, string)) *form {
	return m.openFormOpts(title, fields, onSubmit, formOpts{})
}

// openFormScrimmed is openForm for the surfaces the requirement singles out:
// the backdrop fades because the operator has nothing else to do until this is
// answered. Login is one; everything else keeps the live backdrop, because the
// operator is usually reading the thing behind the dialog.
func (m *Model) openFormScrimmed(title string, fields []formField, onSubmit func(formValues) (bool, string)) *form {
	return m.openFormOpts(title, fields, onSubmit, formOpts{scrim: true})
}

func (m *Model) openFormOpts(title string, fields []formField,
	onSubmit func(formValues) (bool, string), opts formOpts) *form {
	fm := newForm(fields, onSubmit, opts.prose)
	okText := opts.okText
	if okText == "" {
		okText = "OK"
	}
	// NO MNEMONIC ON THE AFFIRMATIVE. It used to carry 'O', which made a bare
	// `o` activate it -- and `o` is a character an operator types into a name,
	// a CIDR or a passphrase. Enter on the last field already reaches this
	// button, and Tab reaches it from anywhere, so the mnemonic bought nothing
	// and cost a letter.
	// THE TWO BUTTONS ARE THE SAME WIDTH. "OK" beside "Cancel" rendered as a
	// stub next to a slab, which reads as one real control and one afterthought
	// -- and the affirmative was the stub.
	labels := padButtonLabels([]string{okText, "Cancel"})
	fm.ok = widget.NewButton(labels[0],
		widget.WithRole(widget.ButtonRoleDefault),
		widget.WithOnActivate(fm.submit))
	// THE CANCEL BUTTON DISMISSES ITSELF. Escape resolves the cancel ROLE and
	// closes the dialog for you, but activating the button — clicking it, or
	// Enter on it — runs its callback and nothing else. Without this, Escape
	// worked and the button the operator can see did nothing.
	var md *widget.Modal
	cancel := widget.NewButton(labels[1],
		widget.WithRole(widget.ButtonRoleCancel),
		widget.WithMnemonic('C'),
		widget.WithOnActivate(func() {
			// Dismissing is ALL this does; opts.onCancel is reached through
			// the dismissal hook, which Escape reaches too.
			if md != nil {
				md.Dismiss(widget.DismissCancel)
			}
		}))
	// WithScrim is passed at BOTH ends on purpose. Modal defaults it to true —
	// the inverse of Float's, which defaults to no scrim — so leaving it out
	// would fade the backdrop behind every dialog by doing nothing, which is
	// the opposite of what this product asked for.
	// NO DISMISS KEY. `q` is a typed character in a form — a CIDR, a note name,
	// a passphrase, a PAT label — and a form that closed on it could not accept
	// one. dismissKey records this exclusion as permanent, and it survives the
	// conversion to a dialog unchanged. Escape still closes.
	buttons := []*widget.Button{fm.ok}
	if !opts.noCancel {
		buttons = append(buttons, cancel)
	}
	md = widget.NewModal(fm,
		widget.WithModalTitle(title),
		widget.WithButtons(buttons...),
		widget.WithScrim(opts.scrim))
	if err := md.Open(m.host); err != nil {
		m.setError("open " + title + ": " + err.Error())
		return fm
	}
	fm.surface = modalOverlay{m: md}
	m.trackOverlay(fm.surface, fm, title, func() {
		if fm.submitted || opts.onCancel == nil {
			return
		}
		opts.onCancel(fm.values())
	})
	// THE FIRST FIELD TAKES THE KEYBOARD, not the default button. Modal seeds
	// focus onto the affirmative button, which is right for a confirmation and
	// wrong for a form: the operator opened this to type, and would otherwise
	// have to Tab backwards to reach the first thing they came for.
	if len(fm.fields) > 0 && m.ctx != nil {
		m.ctx.FocusComponent(fm.fields[0].ctl.component())
	}
	return fm
}

// leaderEntry is one binding in the Space menu.
type leaderEntry struct {
	key   rune
	label string
	// run == nil marks a DISABLED row: shown, with its reason already folded
	// into label, and its key does nothing. A command whose moment has not come
	// teaches more visible than hidden, and hiding it makes the menu shift under
	// the operator between openings.
	//
	// Encoded as a nil handler rather than a flag so the dozen unkeyed
	// leaderEntry literals across this package keep compiling; "there is
	// nothing to run" is also the honest reading of the state.
	run func()
}

// leaderMenu is the which-key float: one more keypress executes and
// dismisses; Esc cancels (the Float's own modal Esc handling).
type leaderMenu struct {
	widget.Base
	entries []leaderEntry
	// prose is shown ABOVE the keys, in the SAME float.
	//
	// NOTHING WRITES IT ANY MORE. The confirmations that needed prose beside
	// their keys are dialogs now, and openLeaderWithProse went with them; what
	// is left is the Space menu and the keyslot action list, neither of which
	// carries prose. The field and its rendering stay because the leaderMenu
	// still renders an empty prose correctly and a menu that grows a preamble
	// is a plausible next request — but if that has not happened by the time
	// somebody reads this, delete it.
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
	muted := mutedStyle()
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
	case idx >= 0 && l.entries[idx].run == nil:
		// CONSUMED AND REFUSED, and the float STAYS OPEN. Closing on a dead key
		// would be indistinguishable from having run the command, which is the
		// one reading a disabled row must never produce. Consumed rather than
		// ignored so the key does not fall through to whatever is behind.
		return true
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
