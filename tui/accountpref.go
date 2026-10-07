package tui

import (
	"context"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE ACCOUNT'S PREFERENCES — the editor's keys (Vim or TextEdit) and the
// theme, each as the signed-in account prefers. Both are an accountPref, and
// what follows holds for each.
//
// Options › Editor or Options › Theme changes the screen at once — the
// buffer, the cursor and the undo history stay — and then stores the choice on
// the account. The screen changes whether or not the store accepts it: a
// failed write costs the persistence, not the change.
//
// ONE WRITE AT A TIME. Two in flight can land in either order and the store
// cannot tell which was chosen last, so one goes out; a choice made meanwhile
// waits, replacing any other waiting one — the latest answer is the one worth
// writing. The writer's slot comes back whatever the answer, or no preference
// would ever be written again.
//
// EACH CHOICE BELONGS TO WHOEVER MADE IT: it is written with the credential it
// was made under, reported only to that identity, and a waiting one is dropped
// when that identity signs out.
//
// After a sign-in the account's stored choice is read and applied. None
// stored means the preference's default — Vim, the layout's own theme — not
// whatever the previous account chose. A choice from the menu made while that
// read is out wins over it.

// accountPref is one preference, as shown now, and its writer.
type accountPref struct {
	key   string // its key in the account's options: auth.OptionEditorKeyset
	def   string // what none stored means
	label string // what the status line calls it: "editor", "theme"
	// show puts a value on the screen; it is the preference's own.
	show func(h *Host, value string)

	pref    string // the value shown now
	gen     uint64 // numbers the choices; a read or report for an older one is stale
	ticket  uint64
	active  uint64 // the writer's ticket in flight, 0 for none
	pending *prefIntent
	// write and read reach the account's options: the backend's, unless a
	// test holds them.
	write func(ctx context.Context, b *Bound, pref string) error
	read  func(ctx context.Context, b *Bound) (map[string]string, error)
	// settled counts the reads and writes that came back, applied or not.
	settled int
}

// prefIntent is one choice, whole: what, which choice it was, who made it,
// and the credential it was made under.
type prefIntent struct {
	pref  string
	gen   uint64
	epoch uint64
	bound *Bound
}

func newAccountPref(key, def, label string, show func(h *Host, value string)) *accountPref {
	return &accountPref{
		key: key, def: def, label: label, show: show, pref: def,
		write: func(ctx context.Context, b *Bound, value string) error {
			return b.SetOption(ctx, key, value)
		},
		read: func(ctx context.Context, b *Bound) (map[string]string, error) { return b.Options(ctx) },
	}
}

// newKeysetPref is the editor's keys: Vim unless the account says otherwise.
func newKeysetPref() *accountPref {
	return newAccountPref(auth.OptionEditorKeyset, auth.KeysetVim, "editor", (*Host).showKeyset)
}

// newThemePref is the theme: the layout's own import unless the account says
// otherwise.
func newThemePref(def string) *accountPref {
	return newAccountPref(auth.OptionTheme, def, "theme", func(h *Host, name string) {
		if name != h.theme {
			h.useTheme(name)
		}
	})
}

// newLanguagePref is the language the UI text shows in: English unless the
// account says otherwise. A stored value this build does not offer is not
// applied — the App accepts any tag, so an unlisted one would leave the screen
// on English text with a language the catalogs cannot resolve; English is what
// the value falls back to. The stored option itself is untouched, so a build
// that does offer the language reads it again.
func newLanguagePref() *accountPref {
	return newAccountPref(auth.OptionLanguage, "en", "language", func(h *Host, tag string) {
		h.useLanguage(languageOf(tag))
	})
}

// keysetValue is the document's App.keyset for a stored preference: golib's
// Standard profile is the product's TextEdit.
func keysetValue(pref string) string {
	if pref == auth.KeysetTextEdit {
		return "standard"
	}
	return "vim"
}

// showKeyset puts pref on the editor and on the Editor menu's mark.
func (h *Host) showKeyset(pref string) {
	h.set("App.keyset", keysetValue(pref))
	h.reproject()
}

// showPref puts a value on the screen and records it as shown.
func (h *Host) showPref(p *accountPref, value string) {
	p.pref = value
	p.show(h, value)
}

// choosePref is a choice from a menu: options.editor.vim, options.theme.retro.
func (h *Host) choosePref(p *accountPref, value string) {
	p.gen++
	h.showPref(p, value)
	h.submitPref(p, prefIntent{pref: value, gen: p.gen, epoch: h.idEpoch, bound: h.session.Bind()})
}

// submitPref writes one choice, or keeps it waiting for the one in flight.
func (h *Host) submitPref(p *accountPref, in prefIntent) {
	if p.active != 0 {
		p.pending = &in
		return
	}
	p.ticket++
	ticket := p.ticket
	p.active = ticket
	write := p.write
	type written struct{ err error }
	do(h, func(ctx context.Context) written {
		// in.bound, never the session now: the credential it was chosen under.
		return written{err: write(ctx, in.bound, in.pref)}
	}, func(w written) {
		p.settled++
		if ticket != p.active {
			return // its slot was taken from it (a sign-out); it owns nothing
		}
		p.active = 0
		if in.epoch == h.idEpoch && in.gen == p.gen {
			if w.err != nil {
				h.setStatus(p.label + " set to " + in.pref + " for this session only — saving it failed: " + WireErrorMessage(w.err))
			} else {
				h.setStatus(p.label + ": " + in.pref)
			}
		}
		next := p.pending
		p.pending = nil
		if next != nil && next.epoch == h.idEpoch {
			h.submitPref(p, *next)
		}
	})
}

// applyStoredPrefs reads each preference the account stored, after a sign-in,
// and wears it.
func (h *Host) applyStoredPrefs() {
	for _, p := range h.accountPrefs() {
		h.applyStored(p)
	}
}

// applyStored reads the account's choice for p and wears it, unless the user
// has chosen since.
func (h *Host) applyStored(p *accountPref) {
	gen, epoch := p.gen, h.idEpoch
	bound, read := h.session.Bind(), p.read
	type stored struct {
		opts map[string]string
		err  error
	}
	do(h, func(ctx context.Context) stored {
		opts, err := read(ctx, bound)
		return stored{opts: opts, err: err}
	}, func(s stored) {
		p.settled++
		if epoch != h.idEpoch || gen != p.gen {
			return // another identity, or a choice from the menu since
		}
		if s.err != nil {
			h.setStatus("could not read your preferences: " + WireErrorMessage(s.err))
			return
		}
		value := p.def
		if v, ok := s.opts[p.key]; ok {
			value = v
		}
		// Always shown, never compared with p.pref: the screen may wear
		// something the preference never chose (a theme picked before the
		// sign-in), and the account's answer is what it must wear now. Each
		// show is cheap when nothing changes.
		h.showPref(p, value)
	})
}

// forgetPrefs drops what the departing identity chose: its waiting write, and
// the writer's slot — retired, not waited for, so the next person's choice
// does not queue behind a call belonging to somebody who has signed out — and
// shows each preference's default.
func (h *Host) forgetPrefs() {
	for _, p := range h.accountPrefs() {
		p.gen++
		p.pending = nil
		p.active = 0
		h.showPref(p, p.def)
	}
}

// accountPrefs are the account's preferences, in the order they are read.
func (h *Host) accountPrefs() []*accountPref {
	return []*accountPref{h.prefs, h.themePref, h.languagePref}
}
