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
	"time"

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

// prefCall is one RPC the writer actually made: which Bound carried it, and
// which preference it asked for.
//
// BOTH HALVES, because a cell that records only the Bound cannot tell whether
// the SECOND write carried the latest choice — and a cell that records only the
// preference cannot tell whose credential sent it.
type prefCall struct {
	bound *Bound
	pref  string
}

// recordingWriter replaces the preference RPC with one that records each call
// and blocks the FIRST until released, so a cell can hold one write open while
// the model queues another behind it.
type recordingWriter struct {
	mu      sync.Mutex
	calls   []prefCall
	release chan struct{}
	once    sync.Once
	// blockEvery holds EVERY write, not only the first.
	//
	// THE TWO CELLS WANT OPPOSITE THINGS FROM THE SECOND WRITE, and leaving
	// that implicit made one of them flaky. The latest-pending cell needs the
	// queued second write to RUN, so it can observe the operator's newest
	// choice reaching the RPC. The late-completion cell needs the second write
	// NOT to finish, because it asserts on the writer's state while that write
	// is still out -- and a second call that returned immediately let its
	// completion race the assertion, settle the ticket it was about to check,
	// and clear the slot to zero.
	//
	// That is a race in this harness, not in the writer: the assertion was
	// right and the test simply could not say when the write it was asking
	// about would land. It failed roughly a third of the time under -race,
	// which CI doubles by running -count=2.
	blockEvery bool
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{release: make(chan struct{})}
}

func (w *recordingWriter) write(ctx context.Context, b *Bound, pref string) error {
	w.mu.Lock()
	first := len(w.calls) == 0
	w.calls = append(w.calls, prefCall{bound: b, pref: pref})
	w.mu.Unlock()
	// Only the first call blocks, unless the cell asked for all of them. A
	// queued second write must be able to RUN once released, which is the whole
	// thing the latest-pending cell observes -- but a cell that asserts while a
	// write is still out has to be able to KEEP it out. See blockEvery.
	if first || w.blockEvery {
		select {
		case <-w.release:
		case <-ctx.Done():
		}
	}
	return nil
}

func (w *recordingWriter) seen() []prefCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]prefCall(nil), w.calls...)
}

func (w *recordingWriter) unblock() { w.once.Do(func() { close(w.release) }) }

// awaitCalls waits until the writer has been entered n times. Waiting on an
// OBSERVED call rather than on prefActive is the point: prefActive is set on
// the loop before the task goroutine has reached the writer, so a cell that
// keys off it can assert about an RPC that has not happened.
func (w *recordingWriter) awaitCalls(t *testing.T, n int, what string) []prefCall {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := w.seen(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: the writer was entered %d time(s), want %d",
		what, len(w.seen()), n)
	return nil
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

	// A chooses; the RPC blocks. WAIT FOR THE WRITER TO BE ENTERED, not for
	// prefActive — the flag is set on the loop before the task goroutine has
	// reached the RPC, so keying off it asserts about a call that may not have
	// happened yet.
	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetTextEdit) })
	w.awaitCalls(t, 1, "A's write to reach the RPC")

	// A signs out while it is still unfinished, and B chooses.
	h.on(func() {
		h.m.retireIdentity()
		h.m.chooseEditorKeyset(auth.KeysetVim)
	})

	// THE OBSERVABLE IS THE CALL, not prefActive. B's write does not block, so
	// by the time the loop is asked, B may already have STARTED AND FINISHED —
	// and prefActive would read 0 for the happy path and the broken one alike.
	// An earlier version of this cell asserted prefActive != 0 and failed
	// against working code for exactly that reason.
	calls := w.awaitCalls(t, 2, "B's write to reach the RPC")

	var pending bool
	h.on(func() { pending = h.m.prefPending != nil })
	if pending {
		t.Error("B's choice was left queued rather than dispatched")
	}
	if calls[1].pref != auth.KeysetVim {
		t.Errorf("the second RPC carried %q, want B's choice %q", calls[1].pref, auth.KeysetVim)
	}
}

// 2. A'S LATE COMPLETION CANNOT CLEAR B, and 3. B STILL SETTLES.
func TestPrefWriter_ALateCompletionDoesNotDisturbTheNewWriter(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	// B MUST STILL BE OUT WHEN THIS CELL LOOKS AT THE WRITER. Everything below
	// asks what A's late completion did to B's ticket, which is only a question
	// while B's own completion has not arrived. Letting B finish meant its
	// completion sometimes settled the very ticket the assertion was about to
	// read, and the cell reported a cleared slot as the defect it was looking
	// for -- a failure of the harness wearing the costume of a real finding.
	w.blockEvery = true
	defer close(w.release)
	h.on(func() { h.m.writeOption = w.write })

	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetTextEdit) })
	w.awaitCalls(t, 1, "A's write to reach the RPC")
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
// 4. WITHIN ONE IDENTITY, THE LATEST PENDING CHOICE IS THE ONE ACTUALLY WRITTEN.
//
// THE ASSERTION IS THE SECOND RPC, not the queue. An earlier version of this
// cell inspected prefPending and stopped there, and deleting the
// `m.submitPref(*next)` that drains the queue left it green — the queue held the
// right value and nobody ever sent it. What matters is which preference reaches
// the daemon, so that is what this waits for.
func TestPrefWriter_LatestPendingChoiceIsTheOneWritten(t *testing.T) {
	h := prefModel(t)
	w := newRecordingWriter()
	defer w.unblock()
	h.on(func() { h.m.writeOption = w.write })

	var firstBound *Bound
	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetVim) }) // starts, and blocks
	first := w.awaitCalls(t, 1, "the first write to reach the RPC")
	firstBound = first[0].bound

	h.on(func() {
		h.m.chooseEditorKeyset(auth.KeysetTextEdit) // queues
		h.m.chooseEditorKeyset(auth.KeysetVim)      // replaces the queued one
		h.m.chooseEditorKeyset(auth.KeysetTextEdit) // and again: the LATEST wins
	})

	// Release the first; the drain must now send the queued choice.
	w.unblock()
	calls := w.awaitCalls(t, 2, "the queued write to be dispatched")

	if calls[1].pref != auth.KeysetTextEdit {
		t.Errorf("the second RPC carried %q, want the LATEST choice %q",
			calls[1].pref, auth.KeysetTextEdit)
	}
	// The BOUND is not compared by pointer: Session.Bind() mints a fresh one per
	// call, so two choices by the same person legitimately carry different
	// objects. What must match is the identity inside it, and that the write
	// carried the one captured AT THE CHOICE is the subject of its own cell.
	if calls[1].bound == nil {
		t.Fatal("the second RPC carried no Bound")
	}
	if calls[1].bound.IdentityEpoch() != firstBound.IdentityEpoch() {
		t.Errorf("the second RPC carried identity epoch %d, want %d — the same "+
			"person made both choices",
			calls[1].bound.IdentityEpoch(), firstBound.IdentityEpoch())
	}
	if len(calls) != 2 {
		t.Errorf("the writer ran %d times, want exactly 2 — the intermediate "+
			"choices should have been coalesced away", len(calls))
	}
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

	if got := w.seen()[0].bound; got != chosen {
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

// recordingReader holds a stored-preference read open, so a cell can land it
// AFTER a later menu choice and see which one wins.
type recordingReader struct {
	mu      sync.Mutex
	calls   int
	opts    map[string]string
	release chan struct{}
	once    sync.Once
}

func newRecordingReader(opts map[string]string) *recordingReader {
	return &recordingReader{opts: opts, release: make(chan struct{})}
}

func (r *recordingReader) read(ctx context.Context, b *Bound) (map[string]string, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return r.opts, nil
}

func (r *recordingReader) unblock() { r.once.Do(func() { close(r.release) }) }

func (r *recordingReader) entered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// 7. A DELAYED STORED READ LOSES TO A LATER MENU CHOICE.
//
// THROUGH THE REAL PATH, not through resolveEditorPref. An earlier version of
// the read cells called that helper directly, so disabling the
// `gen != m.prefGen` comparison in applyStoredEditorKeyset left every cell green
// — the ordering rule had no test at all. This one holds the read open, makes a
// choice while it is in flight, releases it through the ordinary task path, and
// asserts the newer choice survives.
func TestPrefRead_ADelayedReadCannotOverwriteALaterChoice(t *testing.T) {
	h := prefModel(t)
	// The stored preference disagrees with what the operator is about to pick,
	// so "the read lost" and "the read never happened" look different.
	r := newRecordingReader(map[string]string{auth.OptionEditorKeyset: auth.KeysetVim})
	defer r.unblock()
	w := newRecordingWriter()
	defer w.unblock()
	h.on(func() { h.m.readOptions, h.m.writeOption = r.read, w.write })

	h.on(func() { h.m.applyStoredEditorKeyset() })
	waitFor(t, "the read to reach the RPC", func() bool { return r.entered() == 1 })

	// The operator chooses while the read is still out. This is the newer intent.
	h.on(func() { h.m.chooseEditorKeyset(auth.KeysetTextEdit) })
	var afterChoice widget.Keyset
	h.on(func() { afterChoice = h.m.editor.Keyset() })
	if afterChoice != widget.KeysetStandard {
		t.Fatalf("the menu choice did not apply: keyset = %v", afterChoice)
	}

	// Now let the stale read land, through the ordinary task path.
	r.unblock()
	h.settle()

	var got widget.Keyset
	h.on(func() { got = h.m.editor.Keyset() })
	if got != widget.KeysetStandard {
		t.Errorf("keyset = %v, want Standard — a read issued BEFORE the choice "+
			"overwrote it", got)
	}
}

// POSITIVE CONTROL: a read with no competing choice DOES apply. Without this,
// the cell above would pass against an applyStoredEditorKeyset that never
// applies anything.
func TestPrefRead_AnUncontestedReadApplies(t *testing.T) {
	h := prefModel(t)
	r := newRecordingReader(map[string]string{auth.OptionEditorKeyset: auth.KeysetTextEdit})
	h.on(func() { h.m.readOptions = r.read })

	h.on(func() { h.m.applyStoredEditorKeyset() })
	waitFor(t, "the read to reach the RPC", func() bool { return r.entered() == 1 })
	r.unblock()
	h.settle()

	var got widget.Keyset
	h.on(func() { got = h.m.editor.Keyset() })
	if got != widget.KeysetStandard {
		t.Errorf("keyset = %v, want Standard — an uncontested stored preference "+
			"was not applied", got)
	}
}

// waitFor polls a condition off the loop. The recording seams are guarded by
// their own mutexes, so they are safe to read from here.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
