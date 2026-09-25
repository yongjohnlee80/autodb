package tui

import (
	"context"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// THE EDITOR'S KEYS — Vim or TextEdit, as the signed-in account prefers.
//
// Options › Editor switches the query editor at once — the buffer, the
// cursor and the undo history stay — and then stores the choice on the
// account. The editor changes whether or not the store accepts it: a failed
// write costs the persistence, not the change.
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
// stored means Vim — not whatever the previous account chose. A choice from
// the menu made while that read is out wins over it.

// editorPrefs is the editor profile and its writer.
type editorPrefs struct {
	pref    string // auth.KeysetVim or auth.KeysetTextEdit, as shown now
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

func newEditorPrefs() *editorPrefs {
	return &editorPrefs{
		pref: auth.KeysetVim,
		write: func(ctx context.Context, b *Bound, pref string) error {
			return b.SetOption(ctx, auth.OptionEditorKeyset, pref)
		},
		read: func(ctx context.Context, b *Bound) (map[string]string, error) { return b.Options(ctx) },
	}
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
	h.prefs.pref = pref
	h.set("App.keyset", keysetValue(pref))
	h.reproject()
}

// chooseKeyset is options.editor.vim / .textedit.
func (h *Host) chooseKeyset(pref string) {
	p := h.prefs
	p.gen++
	h.showKeyset(pref)
	h.submitPref(prefIntent{pref: pref, gen: p.gen, epoch: h.idEpoch, bound: h.session.Bind()})
}

// submitPref writes one choice, or keeps it waiting for the one in flight.
func (h *Host) submitPref(in prefIntent) {
	p := h.prefs
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
				h.setStatus("editor set to " + in.pref + " for this session only — saving it failed: " + WireErrorMessage(w.err))
			} else {
				h.setStatus("editor: " + in.pref)
			}
		}
		next := p.pending
		p.pending = nil
		if next != nil && next.epoch == h.idEpoch {
			h.submitPref(*next)
		}
	})
}

// applyStoredKeyset reads the account's choice after a sign-in and wears it,
// unless the user has chosen since.
func (h *Host) applyStoredKeyset() {
	p := h.prefs
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
		pref := auth.KeysetVim
		if v, ok := s.opts[auth.OptionEditorKeyset]; ok {
			pref = v
		}
		h.showKeyset(pref)
	})
}

// forgetPrefs drops what the departing identity chose: its waiting write, and
// the writer's slot — retired, not waited for, so the next person's choice
// does not queue behind a call belonging to somebody who has signed out.
func (h *Host) forgetPrefs() {
	p := h.prefs
	p.gen++
	p.pending = nil
	p.active = 0
	h.showKeyset(auth.KeysetVim)
}
