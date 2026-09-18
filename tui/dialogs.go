package tui

import (
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Dialogs: a question with answers, as a widget.Modal.
//
// THIS IS NOT A CONVERSION OF openLeader, and that distinction is the whole
// reason this file exists. openLeader is shared by the quit confirmation, the
// front-door prompts, the note-conflict prompts AND the Space menu itself;
// converting the helper would drag the leader menu into being a dialog, which
// it is not. Named surfaces move here one at a time; the helper stays where it
// is, serving the menu.
//
// The panels do not move either. A manager is a LIST the operator works in with
// a/e/d keys, and a Modal is a card with buttons: forcing one into the other
// would add a button row nobody presses and take away nothing.

// dialogAnswer is one button on a dialog.
type dialogAnswer struct {
	// mnemonic is the key that activates it, and it is the same key the leader
	// version of this prompt used, so the muscle memory survives the change.
	mnemonic rune
	label    string
	run      func()
	// role decides Escape and initial focus. Exactly one answer may be
	// ButtonRoleDefault, and some dialogs deliberately have none — see
	// openDialogNoDefault.
	role widget.ButtonRole
}

// affirm is the answer that does the thing, focused by default.
func affirm(mnemonic rune, label string, run func()) dialogAnswer {
	return dialogAnswer{mnemonic: mnemonic, label: label, run: run, role: widget.ButtonRoleDefault}
}

// decline is the answer that does nothing, resolved by Escape.
func decline(mnemonic rune, label string) dialogAnswer {
	return dialogAnswer{mnemonic: mnemonic, label: label, role: widget.ButtonRoleCancel}
}

// alternative is an answer that is neither: a third way out, such as "save as a
// new name" on a conflict.
func alternative(mnemonic rune, label string, run func()) dialogAnswer {
	return dialogAnswer{mnemonic: mnemonic, label: label, run: run}
}

// openDialog asks a question. The backdrop stays live.
func (m *Model) openDialog(title, prose string, answers ...dialogAnswer) *widget.Modal {
	return m.openDialogOpts(title, prose, false, answers)
}

// openDialogScrimmed IS GONE. Quit was its only caller and quit is built from
// the modal factory now, which asks for the scrim with Scrimmed(). A helper
// whose every caller has left is not "available for reuse" -- it is a second
// way to do a thing, kept alive by the test that measures it.

// openDialogNoDefault asks a question whose affirmative must not be one keypress
// away: nothing here may be reached by a bare Enter on an untouched dialog.
//
// REMOVING THE ROLE IS NOT ENOUGH, and an earlier version of this helper got
// that wrong in a way worth recording. It refused a ButtonRoleDefault answer and
// stopped there — but golib's Modal seeds focus on the default button IF THERE
// IS ONE, and otherwise on the FIRST ENABLED BUTTON. Delete and Revoke listed
// their irreversible answer first, so focus landed on it and bare Enter deleted
// the note. The guard checked the ROLE while the affordance is decided by ORDER,
// and the cell that checked the guard agreed with it.
//
// So this does two things instead of one: it requires a declining answer, and it
// puts that answer FIRST, which is what Modal's fallback actually consults. The
// affirmative is still reachable by its mnemonic, by Tab, and by a click — it is
// simply never the thing already under the cursor.
func (m *Model) openDialogNoDefault(title, prose string, answers ...dialogAnswer) *widget.Modal {
	var cancel int = -1
	for i, a := range answers {
		if a.role == widget.ButtonRoleDefault {
			panic("tui: openDialogNoDefault: answer " + string(a.mnemonic) +
				" is ButtonRoleDefault; this surface exists to not have one")
		}
		if a.role == widget.ButtonRoleCancel && cancel < 0 {
			cancel = i
		}
	}
	if cancel < 0 {
		panic("tui: openDialogNoDefault: no declining answer. Modal focuses the " +
			"first enabled button when nothing is default, so a dialog without " +
			"one puts its affirmative under the cursor")
	}
	// The declining answer moves to the front, because FIRST is what Modal's
	// focus fallback means by "safe".
	ordered := make([]dialogAnswer, 0, len(answers))
	ordered = append(ordered, answers[cancel])
	for i, a := range answers {
		if i != cancel {
			ordered = append(ordered, a)
		}
	}
	return m.openDialogOpts(title, prose, false, ordered)
}

func (m *Model) openDialogOpts(title, prose string, scrim bool, answers []dialogAnswer) *widget.Modal {
	body := &dialogBody{}
	if p := strings.TrimRight(prose, "\n"); p != "" {
		body.prose = strings.Split(p, "\n")
	}

	var md *widget.Modal
	// EVERY ANSWER IS THE SAME WIDTH, for the reason padButtonLabels gives:
	// buttons sized by their own text turn "No" beside "Discard and switch"
	// into a stub next to a slab.
	texts := make([]string, len(answers))
	for i, a := range answers {
		texts[i] = a.label
	}
	texts = padButtonLabels(texts)
	buttons := make([]*widget.Button, 0, len(answers))
	for ai, a := range answers {
		run := a.run
		// THE REASON COMES FROM THE ROLE, not from the fact that a button was
		// pressed. Hard-coding Accept made a DECLINING answer report acceptance
		// — and worse, it silently swallowed Escape and `q` too: those resolve
		// through the Cancel-role button, whose callback dismissed first as
		// Accept, leaving golib's own DismissCancel a no-op on an already
		// closed dialog. Anything listening for provenance was told every exit
		// was a yes.
		reason := widget.DismissAccept
		if a.role == widget.ButtonRoleCancel {
			reason = widget.DismissCancel
		}
		opts := []widget.ButtonOption{
			widget.WithMnemonic(a.mnemonic),
			widget.WithRole(a.role),
			// EVERY ANSWER CLOSES THE DIALOG, including the ones that do
			// something. A Modal dismisses itself for Escape and for nothing
			// else: a button activation runs its callback and leaves the card
			// on screen, which is how a confirmation comes to accept a second
			// press of the same answer.
			//
			// DISMISS BEFORE RUN stays: an action that opens another surface
			// must not find this one still on top.
			widget.WithOnActivate(func() {
				if md != nil {
					md.Dismiss(reason)
				}
				if run != nil {
					run()
				}
			}),
		}
		buttons = append(buttons, widget.NewButton(texts[ai], opts...))
	}

	// WithScrim at both ends: Modal defaults it to TRUE, the inverse of Float,
	// so omitting it would fade the backdrop behind every dialog by doing
	// nothing — the opposite of what this product asked for.
	md = widget.NewModal(body,
		widget.WithModalTitle(title),
		widget.WithButtons(buttons...),
		// `q` CLOSES A CONFIRMATION, as it always did. These surfaces were
		// leaderMenu confirmations before the conversion and honoured `q`
		// alongside Escape; a Modal traps focus and swallows it, so becoming a
		// dialog silently took the key away while dismissKey's comment went on
		// promising it. golib v0.5.25 added the option that gives it back.
		//
		// A dialog carries prose and buttons and nothing that accepts typed
		// text, so there is no letter to lose — which is exactly why the FORMS
		// do not declare it. See openFormOpts.
		widget.WithModalDismissKeys('q'),
		widget.WithScrim(scrim))
	if err := md.Open(m.host); err != nil {
		m.setError("open " + title + ": " + err.Error())
		return nil
	}
	m.trackOverlay(modalOverlay{m: md}, body, title, nil)
	return md
}

// dialogBody is the prose above the buttons. A dialog with nothing to say is
// legitimate — "revoke this token?" is answered by its title — and renders as
// an empty body rather than a special case.
type dialogBody struct {
	widget.Base
	ctx   *tui.Context
	prose []string
	text  *widget.Text
}

func (d *dialogBody) Init(ctx *tui.Context) {
	d.Base.Init(ctx)
	d.ctx = ctx
	if len(d.prose) == 0 {
		return
	}
	d.text = widget.NewText(strings.Join(d.prose, "\n"),
		widget.WithWrapMode(widget.Wrap))
	ctx.Mount(d.text)
}

// Layout sizes to the PROSE, not to a fixed fraction.
//
// A consent line that wraps mid-phrase is a consent line nobody read, and the
// leader confirmations this replaced widened themselves for exactly that
// reason. The allowlist prose is pre-formatted with its own line breaks — an
// indented list of CIDRs, a bulleted list of consequences — so a narrower box
// re-wraps text that was already laid out and the result reads as damage.
//
// The screen still wins: whatever is asked for is clamped to what is offered.
func (d *dialogBody) Layout(c tui.Constraints) tui.Size {
	if d.text == nil || d.ctx == nil {
		return tui.Size{}
	}
	cc := c
	want := modalSpan(c.MaxW, dialogPct, dialogMinW, dialogMaxW)
	for _, line := range d.prose {
		if n := len([]rune(line)) + 1; n > want {
			want = n
		}
	}
	cc.MaxW = min(c.MaxW, want)
	sz := d.ctx.LayoutChild(d.text, cc)
	d.ctx.PlaceChild(d.text, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return cc.Constrain(sz)
}

func (d *dialogBody) Render(tui.Surface) {}

// The prose is wide enough to hold a CIDR list without wrapping mid-address,
// which the allowlist consent needs, and capped so a one-line question does not
// stretch across a wide terminal.
const (
	dialogPct  = 50
	dialogMinW = 44
	dialogMaxW = 72
)
