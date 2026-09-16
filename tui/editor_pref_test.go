package tui

// THE EDITOR PREFERENCE'S COORDINATION, one cell per discriminator.
//
// The first version of this code shipped three coordination claims and tested
// none of them, which is how a writer that can never write again and a queue
// that borrows the next person's credential both survived a review of the code
// that contained them. Each cell below drives the Model's own state machine
// directly: the races are between a completion and a later intent, and an
// end-to-end fixture cannot schedule those.

import (
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// prefModel is a REAL Model in a running app, and hand-building one was the
// wrong idea.
//
// A bare &Model{} reaches the early returns but panics the moment a cell gets
// far enough to REPORT: setStatus refreshes the status bar, which needs a
// session and a mounted bar. Cells 1 to 4 all passed against that stub without
// ever touching the reporting path — they would have kept passing against a
// settlePrefWrite that could not report at all. The positive control is what
// exposed it, which is the entire reason to have one.
//
// State is read and written through h.on, because the loop owns it.
func prefModel(t *testing.T) *barHarness {
	t.Helper()
	return startBar(t, meta.RoleAdmin)
}

// 1. THE TICKET ALWAYS COMES BACK.
//
// settlePrefWrite used to live inside a managerReload, which the dispatcher
// DROPS when the connection generation has moved. A dropped completion leaves
// the writer busy forever and no preference is ever written again — the worst
// shape of failure, because the menu keeps accepting choices and none of them
// lands.
func TestPrefWriter_TicketReturnsEvenWhenTheResultIsNoLongerCurrent(t *testing.T) {
	h := prefModel(t)
	var busy bool
	h.on(func() {
		h.m.prefWriting = true
		// A completion from an identity that has since gone: nothing about it
		// is current, and it must STILL free the writer.
		h.m.settlePrefWrite(prefWritten{
			intent: prefIntent{pref: auth.KeysetVim, gen: 1, epoch: 99},
			err:    errors.New("superseded"),
		})
		busy = h.m.prefWriting
	})

	if busy {
		t.Fatal("the writer is still busy after a stale completion; no preference " +
			"can ever be written again")
	}
}

// 2. A QUEUED CHOICE IS NOT WRITTEN UNDER SOMEBODY ELSE'S CREDENTIAL.
//
// The queue outlives the sign-in that filled it. Retiring an identity discards
// what it had waiting, rather than letting the next completion dispatch it.
func TestPrefWriter_RetiringAnIdentityDiscardsItsQueuedChoice(t *testing.T) {
	h := prefModel(t)
	var pending bool
	h.on(func() {
		h.m.prefWriting = true
		queued := prefIntent{pref: auth.KeysetTextEdit, gen: 2, epoch: h.m.identityEpoch}
		h.m.prefPending = &queued
		h.m.retireIdentity()
		pending = h.m.prefPending != nil
	})

	if pending {
		t.Error("a queued preference survived the sign-out that orphaned it")
	}
}

// 3. AND EVEN IF ONE IS QUEUED, IT IS NOT DISPATCHED FOR A DEPARTED IDENTITY.
//
// Belt and braces, deliberately: retireIdentity clears the queue, and the
// dispatcher checks the epoch again. The first is the rule; the second is what
// holds if some future path queues without going through retireIdentity.
func TestPrefWriter_DoesNotDispatchAQueuedChoiceFromAnotherIdentity(t *testing.T) {
	h := prefModel(t)
	var busy, pending bool
	h.on(func() {
		h.m.prefWriting = true
		orphan := prefIntent{pref: auth.KeysetTextEdit, gen: 2, epoch: h.m.identityEpoch + 7}
		h.m.prefPending = &orphan
		h.m.settlePrefWrite(prefWritten{intent: prefIntent{gen: 1, epoch: h.m.identityEpoch}})
		busy, pending = h.m.prefWriting, h.m.prefPending != nil
	})

	if busy {
		t.Error("an orphaned queued choice was dispatched under the current identity")
	}
	if pending {
		t.Error("the orphaned choice is still queued")
	}
}

// 4. A STALE COMPLETION SAYS NOTHING.
//
// Reporting "saving it failed" for a choice the operator has already replaced
// is noise about a decision they have moved on from — and worse, it contradicts
// what the editor is currently doing.
func TestPrefWriter_StaleCompletionDoesNotReportStatus(t *testing.T) {
	h := prefModel(t)
	var msg string
	h.on(func() {
		h.m.statusMsg = ""
		h.m.prefGen, h.m.prefWriting = 5, true
		h.m.settlePrefWrite(prefWritten{
			intent: prefIntent{pref: auth.KeysetVim, gen: 3, epoch: h.m.identityEpoch},
			err:    errors.New("nope"),
		})
		msg = h.m.statusMsg
	})

	if msg != "" {
		t.Errorf("status = %q; a superseded choice reported a failure", msg)
	}
}

// 5. THE CURRENT COMPLETION DOES REPORT, so cell 4 is about staleness rather
// than about the reporting being broken.
func TestPrefWriter_CurrentCompletionReportsFailure(t *testing.T) {
	h := prefModel(t)
	var msg string
	h.on(func() {
		h.m.statusMsg = ""
		h.m.prefGen, h.m.prefWriting = 5, true
		h.m.settlePrefWrite(prefWritten{
			intent: prefIntent{pref: auth.KeysetTextEdit, gen: 5, epoch: h.m.identityEpoch},
			err:    errors.New("nope"),
		})
		msg = h.m.statusMsg
	})

	if msg == "" {
		t.Fatal("a current failure reported nothing")
	}
}

// 6. A MISSING PREFERENCE IS THE DEFAULT, NOT THE LAST PERSON'S CHOICE.
//
// This is the leak itself, at the one place the decision is made. The read path
// needs a server, so the mapping is asserted where it is decided: an absent key
// resolves to Vim rather than to whatever the editor happens to be set to.
func TestPrefRead_MissingPreferenceResolvesToVimNotTheEditorsCurrentState(t *testing.T) {
	h := prefModel(t)
	var got widget.Keyset
	h.on(func() {
		h.m.editor.SetKeyset(widget.KeysetStandard) // the previous account's choice
		// THE REAL FUNCTION, not a copy of its logic. An earlier draft of this
		// cell inlined the lookup, which would have agreed with itself no
		// matter what the product did.
		h.m.editor.SetKeyset(keysetOf(resolveEditorPref(map[string]string{})))
		got = h.m.editor.Keyset()
	})

	if got != widget.KeysetVim {
		t.Errorf("keyset = %v, want Vim — an account with no preference inherited "+
			"the previous account's editor", got)
	}
}

// And a stored preference is honoured, so cell 6 is about the MISSING case
// rather than about resolveEditorPref returning Vim unconditionally.
func TestPrefRead_AStoredPreferenceIsHonoured(t *testing.T) {
	got := resolveEditorPref(map[string]string{auth.OptionEditorKeyset: auth.KeysetTextEdit})
	if got != auth.KeysetTextEdit {
		t.Errorf("resolveEditorPref = %q, want %q", got, auth.KeysetTextEdit)
	}
}
