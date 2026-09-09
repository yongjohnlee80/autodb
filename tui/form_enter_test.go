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
func TestForm_EnterAdvancesAndOnlyTheLastFieldSubmits(t *testing.T) {
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

	// The LAST field submits, and a closed float is that claim in full.
	h.keys("sqlite")
	h.key(tuicore.KeyEnter)
	h.keys("file:formenter?mode=memory&cache=shared")
	h.key(tuicore.KeyEnter)
	h.waitGone("connection form", "new connection")
}

// THE FOOTER MUST SAY SO, and say what the form actually does.
//
// The shared vocabulary claimed {"Enter","submit"} while Enter submitted from
// any field; now Enter advances unless the last field holds focus, and one
// definition feeds both the footer and the `?` overlay so they cannot drift.
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
		"Tab/Enter:next  Enter:submit on last  Esc:cancel")
}

// A ONE-FIELD FORM STILL SUBMITS ON ENTER.
//
// Index 0 is also the last index, so the single-field case is correct by
// construction rather than by a special case — but it is the case most likely
// to be broken by a change to "advance", and search is the form people use
// most, so it gets its own cell.
func TestForm_SingleFieldStillSubmitsOnEnter(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	// The editor pane, then search in it: one field, Enter submits.
	h.leader("q")
	h.keys("iSELECT 1")
	h.key(tuicore.KeyEscape)
	h.keys("/")
	h.waitFor("search form", "search in")
	h.keys("SELECT")
	h.key(tuicore.KeyEnter)
	h.waitGone("search form", "search in")
}

// A VALIDATION FAILURE KEEPS THE FORM OPEN AND SAYS WHY.
//
// The submit path reports either "close" or "stay with a message", and the
// message is the only thing that tells an operator what to correct. Enter now
// reaching submit only on the last field must not have changed that.
func TestForm_ValidationFailureKeepsTheFormOpen(t *testing.T) {
	h := startUI(t, startRealServer(t))
	bootstrapRoot(t, h)

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	// Empty name, tab to the last field, submit.
	h.key(tuicore.KeyEnter) // advance (empty name)
	h.key(tuicore.KeyEnter) // advance (empty engine)
	h.key(tuicore.KeyEnter) // last field: submit, with everything empty
	h.waitFor("the form stayed open", "new connection")
	h.waitFor("and said what was wrong", "name")
}

// focusChanges counts input-focus moves seen so far. The runtime trace is the
// evidence for a focus claim; the screen is not, because a form shows its
// labels whether or not a field is filled.
func (h *uiHarness) focusChanges() int {
	return strings.Count(h.trace.tail(4000), "focus node=*widget.TextInput")
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
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")
}
