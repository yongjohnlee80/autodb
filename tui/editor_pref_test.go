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
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
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
//
// THE FIRST SET OF THESE CELLS BYPASSED BOTH PRODUCTION SEAMS, and a reviewer
// proved it with two mutations that every one of them survived: deleting
// applyTask's whole `case prefWritten` branch, and swapping the captured Bound
// for m.session.Bind(). They called settlePrefWrite directly and never issued a
// write, so neither the dispatcher wiring nor the credential capture was under
// test. The cells below enter through applyTask and through submitPref with an
// observable writer.
func prefModel(t *testing.T) *barHarness {
	t.Helper()
	return startBar(t, meta.RoleAdmin)
}

// deliverPref feeds a completion the way the runtime does — through applyTask —
// so the dispatcher wiring is under test rather than assumed. Calling
// settlePrefWrite directly is what let a mutation delete the whole
// `case prefWritten` branch with every cell still green.
func deliverPref(h *barHarness, v prefWritten) {
	h.on(func() { h.m.applyTask(tuicore.TaskResult{Value: v}) })
}

// recordingWriter replaces the preference RPC with one that records WHICH Bound
// it was handed and blocks until released, so a cell can hold a write open.
type recordingWriter struct {
	mu      sync.Mutex
	bounds  []*Bound
	release chan struct{}
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{release: make(chan struct{})}
}

func (w *recordingWriter) write(ctx context.Context, b *Bound, pref string) error {
	w.mu.Lock()
	w.bounds = append(w.bounds, b)
	w.mu.Unlock()
	select {
	case <-w.release:
	case <-ctx.Done():
	}
	return nil
}

func (w *recordingWriter) seen() []*Bound {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*Bound(nil), w.bounds...)
}

// 1. B STARTS WHILE A IS STILL BLOCKED.
//
// This is the defect the ticket exists for. retireIdentity cleared the queue and
// the generation but left the writer owned, so if A's RPC never returned, every
// later choice queued behind it forever.
func TestPrefWriter_RetirementFreesTheWriterForTheNextIdentity(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	defer close(w.release)
	h.on(func() { h.m.writeOption = w.write })

	// A chooses; the RPC blocks.
	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetTextEdit) })
	h.waitUntil("A's write is in flight", func() bool { return h.m.prefActive != 0 })

	// A signs out while it is still unfinished, and B chooses.
	h.on(func() {
		h.m.retireIdentity()
		h.m.chooseEditorKeyset(auth.KeysetVim)
	})

	var active uint64
	var pending bool
	h.on(func() { active, pending = h.m.prefActive, h.m.prefPending != nil })
	if active == 0 {
		t.Fatal("B's choice never started; the writer is still owned by a departed identity")
	}
	if pending {
		t.Error("B's choice was queued rather than started")
	}
	if len(w.seen()) != 2 {
		t.Errorf("the RPC ran %d times, want 2 — B never reached it", len(w.seen()))
	}
}

// 2. A'S LATE COMPLETION CANNOT CLEAR B, and 3. B STILL SETTLES.
func TestPrefWriter_ALateCompletionDoesNotDisturbTheNewWriter(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	defer close(w.release)
	h.on(func() { h.m.writeOption = w.write })

	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetTextEdit) })
	h.waitUntil("A's write is in flight", func() bool { return h.m.prefActive != 0 })
	var aTicket uint64
	h.on(func() { aTicket = h.m.prefActive })

	h.on(func() {
		h.m.retireIdentity()
		h.m.chooseEditorKeyset(auth.KeysetVim)
	})
	var bTicket uint64
	h.on(func() { bTicket = h.m.prefActive })
	if bTicket == aTicket {
		t.Fatalf("B reused A's ticket %d; they must be distinct", aTicket)
	}

	// A's abandoned completion arrives, through the dispatcher.
	deliverPref(h, prefWritten{ticket: aTicket, intent: prefIntent{pref: auth.KeysetTextEdit}})

	var active uint64
	h.on(func() { active = h.m.prefActive })
	if active != bTicket {
		t.Errorf("prefActive = %d after A's late completion, want B's ticket %d — "+
			"a departed write freed a slot somebody else was using", active, bTicket)
	}

	// And B's own completion still settles it.
	deliverPref(h, prefWritten{ticket: bTicket, intent: prefIntent{pref: auth.KeysetVim, epoch: h.m.identityEpoch}})
	h.on(func() { active = h.m.prefActive })
	if active != 0 {
		t.Errorf("prefActive = %d after B settled, want 0", active)
	}
}

// 4. WITHIN ONE IDENTITY, THE LATEST PENDING CHOICE WINS.
func TestPrefWriter_LatestPendingChoiceIsTheOneWritten(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	h.on(func() { h.m.writeOption = w.write })

	h.on(func() {
		h.m.chooseEditorKeyset(auth.KeysetVim) // starts
	})
	h.waitUntil("the first write is in flight", func() bool { return h.m.prefActive != 0 })
	h.on(func() {
		h.m.chooseEditorKeyset(auth.KeysetTextEdit) // queues
		h.m.chooseEditorKeyset(auth.KeysetVim)      // replaces the queued one
	})

	var queued string
	h.on(func() {
		if h.m.prefPending != nil {
			queued = h.m.prefPending.pref
		}
	})
	if queued != auth.KeysetVim {
		t.Errorf("queued %q, want the LATEST choice %q", queued, auth.KeysetVim)
	}
	close(w.release)
}

// 5. THE CAPTURED BOUND IS THE ONE WRITTEN WITH.
//
// The mutation this catches: swapping in.bound for m.session.Bind(), which
// reintroduces credential rebinding. Nothing that cannot see the Bound the RPC
// received can catch it.
func TestPrefWriter_WritesWithTheBoundCapturedAtChoice(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	defer close(w.release)
	h.on(func() { h.m.writeOption = w.write })

	var chosen *Bound
	h.on(func() {
		chosen = h.m.session.Bind()
		h.m.submitPref(prefIntent{
			pref: auth.KeysetTextEdit, gen: h.m.prefGen,
			epoch: h.m.identityEpoch, bound: chosen,
		})
	})
	h.waitUntil("the RPC ran", func() bool { return len(w.seen()) == 1 })

	if got := w.seen()[0]; got != chosen {
		t.Errorf("the write used a different Bound than the one captured at choice; "+
			"got %p want %p", got, chosen)
	}
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
		h.m.prefActive, h.m.prefTicket = 1, 1
		// A completion from an identity that has since gone: nothing about it
		// is current, and it must STILL free the writer.
		h.m.settlePrefWrite(prefWritten{
			ticket: 1, intent: prefIntent{pref: auth.KeysetVim, gen: 1, epoch: 99},
			err: errors.New("superseded"),
		})
		busy = h.m.prefActive != 0
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
		h.m.prefActive, h.m.prefTicket = 1, 1
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
		h.m.prefActive, h.m.prefTicket = 1, 1
		orphan := prefIntent{pref: auth.KeysetTextEdit, gen: 2, epoch: h.m.identityEpoch + 7}
		h.m.prefPending = &orphan
		h.m.settlePrefWrite(prefWritten{ticket: 1, intent: prefIntent{gen: 1, epoch: h.m.identityEpoch}})
		busy, pending = h.m.prefActive != 0, h.m.prefPending != nil
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
		h.m.prefGen, h.m.prefActive, h.m.prefTicket = 5, 1, 1
		h.m.settlePrefWrite(prefWritten{
			ticket: 1, intent: prefIntent{pref: auth.KeysetVim, gen: 3, epoch: h.m.identityEpoch},
			err: errors.New("nope"),
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
		h.m.prefGen, h.m.prefActive, h.m.prefTicket = 5, 1, 1
		h.m.settlePrefWrite(prefWritten{
			ticket: 1, intent: prefIntent{pref: auth.KeysetTextEdit, gen: 5, epoch: h.m.identityEpoch},
			err: errors.New("nope"),
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
