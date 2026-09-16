package tui_test

import (
	"strings"
	"testing"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// ENTER MUST NOT SUBMIT A FORM THE OPERATOR HAS NOT FINISHED.
//
// widget.TextInput publishes SubmitEvent on Enter, and the form treated every
// one as a submit — so typing a value and pressing Enter, which is the obvious
// way to reach the next field, submitted a half-filled form. Nothing on screen
// said which key advanced. An operator hit this on the create-token form and
// had no way to know what to do.
//
// Driven through the real form, because this is a keybinding: the defect lives
// in the wiring between the widget's event and the form's reaction.
// stillThere fails if want LEAVES the screen within d — the positive form of
// neverAppears, for a claim about something that must persist.
func stillThere(t *testing.T, h *uiHarness, what, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !strings.Contains(h.screen(), want) {
			t.Fatalf("%s vanished (%q left the screen):\n%s", what, want, h.screen())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestForm_EnterAdvancesAndNoFieldSubmits(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")

	// Field 1 of 3. Enter here must ADVANCE, not submit.
	focusBefore := h.focusChanges()
	h.keys("demo")
	h.key(tuicore.KeyEnter)

	// FINDING THE RIGHT SIGNAL TOOK TWO TRIES, and both wrong ones are worth
	// recording because each looked convincing:
	//
	//   - "the form is still open" does not discriminate: a premature submit
	//     FAILS VALIDATION and leaves it open too.
	//   - "the engine field now contains sqlite" does not either: the word
	//     appears in that field's own LABEL, "engine (postgres | mysql |
	//     sqlite)", so the assertion passed against a label.
	//
	// What actually differs is the VALIDATION MESSAGE. An advance produces no
	// status at all; a submit of one filled field out of three produces "all
	// fields are required" immediately.
	h.waitFor("form still open after Enter on field 1", "new connection")
	// Polled, not sampled: reading the screen immediately after the keystroke
	// races the next frame, so a single look found nothing under a mutation
	// that submits. The claim is "this never appears", which needs a window.
	neverAppears(t, h, "the validation message from a premature submit",
		"all fields are required", 750*time.Millisecond)

	// AND FOCUS REALLY MOVED. Observed in the runtime trace rather than on
	// the screen, because every value this form accepts for "engine" --
	// sqlite, postgres, mysql -- appears in that field's OWN LABEL, so a
	// screen match cannot tell a filled field from its prompt.
	h.waitForFocusChange("focus moved to the next field", focusBefore)

	h.chooseOption(engineSQLite)
	h.key(tuicore.KeyTab)
	h.keys("file:formenter?mode=memory&cache=shared")

	// THE LAST FIELD DOES NOT SUBMIT EITHER. Enter here moves to OK, and the
	// form is still open afterwards -- that is the whole change, and it is
	// what frees Enter for a select to open its options instead.
	//
	// THE WITNESS IS THE DIALOG'S OWN TITLE, held across a window. An earlier
	// draft watched for "logged in as root" to NOT appear, which proved
	// nothing: the bootstrap had already put that line on screen, so the
	// assertion was true before the keystroke and would have stayed true under
	// a mutation that submitted.
	h.key(tuicore.KeyEnter)
	h.waitFor("form still open after Enter on the LAST field", "new connection")
	stillThere(t, h, "the connection dialog", "new connection", 400*time.Millisecond)

	// OK submits, and a closed dialog is that claim in full.
	h.key(tuicore.KeyEnter)
	h.waitGone("connection form", "new connection")
}

// THE FOOTER MUST SAY SO, and say what the form actually does.
//
// The shared vocabulary claimed {"Enter","submit"} while Enter submitted from
// any field, then claimed "submit on last" while the last field submitted. Now
// no field submits at all — OK does — and one definition feeds both the footer
// and the `?` overlay so they cannot drift.
func TestForm_FooterNamesTheKeysThatWork(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")

	// Asserted as ONE unwrapped line: a footer that wraps mid-phrase reads
	// worse than none, and the earlier wording did exactly that.
	h.waitFor("the footer, on one line",
		"Tab/Enter:next  O:OK  Esc:cancel")
}

// A ONE-FIELD FORM GOES THROUGH THE BUTTON TOO.
//
// It used to submit on Enter because index 0 is also the last index, and that
// was the one case a reader could mistake for a rule. There is no rule now: the
// single field advances to OK exactly like the last field of any other form, so
// search — the form people use most — behaves like the rest rather than being
// the exception that happens to work.
func TestForm_SingleFieldNeedsTheButtonToo(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	h.leader("q")
	h.keys("iSELECT 1")
	h.key(tuicore.KeyEscape)
	h.keys("/")
	h.waitFor("search form", "search in")
	h.keys("SELECT")

	// One Enter is NOT a submit, even here.
	h.key(tuicore.KeyEnter)
	h.waitFor("search form still open after one Enter", "search in")

	// The second reaches OK.
	h.key(tuicore.KeyEnter)
	h.waitGone("search form", "search in")
}

// A VALIDATION FAILURE KEEPS THE FORM OPEN AND SAYS WHY.
//
// The submit path reports either "close" or "stay with a message", and the
// message is the only thing that tells an operator what to correct. Routing
// every submit through OK must not have changed that.
func TestForm_ValidationFailureKeepsTheFormOpen(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	// Empty name, walk to the button, submit with everything empty.
	h.key(tuicore.KeyEnter) // advance (empty name)
	h.key(tuicore.KeyEnter) // advance (empty engine)
	h.key(tuicore.KeyEnter) // last field: move to OK
	h.key(tuicore.KeyEnter) // OK: submit
	h.waitFor("the form stayed open", "new connection")
	h.waitFor("and said what was wrong", "name")
}

// focusChanges counts input-focus moves seen so far. The runtime trace is the
// evidence for a focus claim; the screen is not, because a form shows its
// labels whether or not a field is filled.
//
// A SELECT COUNTS AS A FIELD. This used to count TextInput alone, and when the
// engine row became a select the advance onto it stopped being visible here --
// the focus moved, the counter could not see it, and the cell failed against
// working code. A counter that recognises only one kind of field measures the
// widget, not the claim.
func (h *uiHarness) focusChanges() int {
	t := h.trace.tail(4000)
	return strings.Count(t, "focus node=*widget.TextInput") +
		strings.Count(t, "focus node=*widget.Select")
}

// waitForFocusChange waits until at least one further input focus move lands.
func (h *uiHarness) waitForFocusChange(what string, before int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.focusChanges() > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("%s: no input focus move after the keystroke.\nlast trace:\n%s",
		what, h.trace.tail(30))
}

// neverAppears fails if sub shows up on the virtual screen within d. Use it
// for absence claims: waitFor's opposite samples once and races the renderer.
func neverAppears(t *testing.T, h *uiHarness, what, sub string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(h.screen(), sub) {
			t.Fatalf("%s appeared (%q):\n%s", what, sub, h.screen())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bootstrapRoot takes a fresh UI through the splash and first-run form.
func bootstrapRoot(t *testing.T, h *uiHarness) {
	t.Helper()
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	// TWO Enters, and the second one is the submit. NO FIELD SUBMITS any more:
	// Enter on the last field moves to OK, and Enter on OK activates it. The
	// first of these used to be the whole submit, which is why this helper is
	// where a form-contract change shows up across the entire suite.
	h.key(tuicore.KeyEnter)
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")
}
