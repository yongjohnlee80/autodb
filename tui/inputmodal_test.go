package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// typeText sends a word to whatever holds the keyboard.
func typeText(h *barHarness, s string) {
	for _, r := range s {
		h.key(r)
	}
}

// A PROSE ROW DOES NOT TAKE THE CURSOR, and that is why it is not a field.
//
// The first shape of this factory gave prose a formField of its own, which put
// it in f.fields — and f.fields is both the focus order and the thing Enter
// walks. Enter from the first input would then land the operator on a line of
// text they cannot type into, and the word meant for the second input would go
// nowhere. The modal would submit with an empty value and nothing on screen
// would say why.
func TestInputModal_ProseDoesNotTakeTheCursor(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var first, second string
	h.on(func() {
		NewInputModal(h.m, "two fields",
			NewTextInput("first", &first),
			NewTextValue("a line of prose, between the two inputs"),
			NewTextInput("second", &second),
		).WithSubmitFn(func(ModalResponse) error { return nil }).Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })

	typeText(h, "one")
	h.key(tuicore.KeyEnter) // advance: must reach the SECOND INPUT, not the prose
	typeText(h, "two")
	h.key(tuicore.KeyEnter) // last field: focus moves to OK
	h.key(tuicore.KeyEnter) // activate OK
	h.waitUntil("the modal submitted and closed", func() bool { return !h.m.modalOpen() })

	if first != "one" || second != "two" {
		t.Fatalf("bindings are %q/%q, want \"one\"/\"two\" — the advance did not reach the second INPUT", first, second)
	}
}

// PROSE IS STILL SHOWN. The cell above would also pass if prose were dropped
// on the floor, so the thing it is there for is asserted separately.
func TestInputModal_ProseIsRendered(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		NewConfirmModal(h.m, "quit", "there are unsaved changes").Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })
	h.settle()

	if !strings.Contains(h.screen(), "there are unsaved changes") {
		t.Fatalf("the question is not on screen:\n%s", h.screen())
	}
}

// THE CANCEL CALLBACK RUNS HOWEVER THE OPERATOR DECLINES.
//
// Escape and the button are covered either way -- widget.Modal resolves a
// dismiss request by activating the Cancel-ROLE button -- so they are here to
// pin the behaviour, not because a defect ever lived in them.
//
// THE THIRD ROW IS THE ONE THAT MEASURES SOMETHING. A modal built with
// ExcludeCancel has no cancel button for Modal to resolve against, so Escape
// takes the other arm of that branch and a callback hung off the button would
// never run at all: an acknowledgement would close with the caller's cleanup
// silently skipped. Wiring the callback to the DISMISSAL covers this case and
// the other two with one wire.
func TestInputModal_CancelFiresHoweverTheOperatorDeclines(t *testing.T) {
	for _, tc := range []struct {
		how string
		act func(h *barHarness)
	}{
		{"Escape", func(h *barHarness) { h.key(tuicore.KeyEscape) }},
		{"the Cancel button", func(h *barHarness) {
			h.key(tuicore.KeyTab) // off the field, onto OK
			h.key(tuicore.KeyTab) // onto Cancel
			h.key(tuicore.KeyEnter)
		}},
	} {
		t.Run(tc.how, func(t *testing.T) {
			h := startBar(t, meta.RoleAdmin)
			var bind string
			cancelled := make(chan ModalResponse, 1)
			h.on(func() {
				NewInputModal(h.m, "name it", NewTextInput("name", &bind)).
					WithCancelFn(func(r ModalResponse) { cancelled <- r }).
					Open()
			})
			h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })
			typeText(h, "typed")

			tc.act(h)
			h.waitUntil("the modal closed", func() bool { return !h.m.modalOpen() })

			select {
			case r := <-cancelled:
				if r.Status != StatusCancelled {
					t.Fatalf("status is %v, want cancelled", r.Status)
				}
			default:
				t.Fatalf("declining by %s ran no cancel callback", tc.how)
			}
			// AND NO BINDING WAS WRITTEN. A cancelled modal must not change
			// the caller's data, however much was typed into it.
			if bind != "" {
				t.Fatalf("a cancelled modal wrote %q through the binding", bind)
			}
		})
	}
}

// A SUCCESSFUL SUBMIT IS NOT A CANCEL. The form hides itself on submit, and
// hiding publishes the same dismissal Escape does — so without the submitted
// flag the cancel callback runs on every successful submit, and a caller that
// uses it to roll something back would undo the change it just made.
func TestInputModal_SubmitDoesNotFireCancel(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var bind string
	cancelled := make(chan struct{}, 1)
	submitted := make(chan struct{}, 1)
	h.on(func() {
		NewInputModal(h.m, "name it", NewTextInput("name", &bind)).
			WithSubmitFn(func(ModalResponse) error { submitted <- struct{}{}; return nil }).
			WithCancelFn(func(ModalResponse) { cancelled <- struct{}{} }).
			Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })
	typeText(h, "kept")
	h.key(tuicore.KeyEnter) // last field: focus to OK
	h.key(tuicore.KeyEnter) // activate
	h.waitUntil("the modal closed", func() bool { return !h.m.modalOpen() })
	h.settle()

	select {
	case <-submitted:
	default:
		t.Fatal("the submit callback never ran")
	}
	select {
	case <-cancelled:
		t.Fatal("a successful submit ran the CANCEL callback")
	default:
	}
	if bind != "kept" {
		t.Fatalf("binding is %q, want \"kept\"", bind)
	}
}

// A REFUSED ROW KEEPS THE MODAL OPEN AND NAMES ITSELF.
//
// Both halves matter. A modal that closes on a bad value has discarded what
// the operator typed, and a message that says only "invalid" leaves them
// hunting the row in a form with five of them.
func TestInputModal_ValidationNamesTheRowAndKeepsItOpen(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var host, port string
	h.on(func() {
		NewInputModal(h.m, "endpoint",
			NewTextInput("host", &host).Required(),
			NewTextInput("port", &port).Required(),
		).WithSubmitFn(func(ModalResponse) error { return nil }).Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })

	typeText(h, "db.example.org")
	h.key(tuicore.KeyEnter) // to port, left empty
	h.key(tuicore.KeyEnter) // to OK
	h.key(tuicore.KeyEnter) // activate
	h.settle()

	var open bool
	h.on(func() { open = h.m.modalOpen() })
	if !open {
		t.Fatal("a refused value closed the modal, discarding what was typed")
	}
	if got := h.screen(); !strings.Contains(got, "port") || !strings.Contains(got, ErrEmptyText.Error()) {
		t.Fatalf("the status does not name the offending row:\n%s", got)
	}
	// NOTHING WAS COMMITTED. Writing the rows that passed would leave the
	// caller's struct half-updated under a modal that is still open.
	if host != "" {
		t.Fatalf("a refused submit committed %q through an earlier row's binding", host)
	}
}

// A SUBMIT ERROR BEHAVES LIKE A VALIDATION FAILURE. A name the server refuses
// as already taken must leave the operator in front of the value they typed,
// not close the modal on a change that did not happen.
func TestInputModal_SubmitErrorKeepsTheModalOpen(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var name string
	h.on(func() {
		NewInputModal(h.m, "new workspace", NewTextInput("name", &name).Required()).
			WithSubmitFn(func(ModalResponse) error {
				return errors.New("that name is already taken")
			}).Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })
	typeText(h, "staging")
	h.key(tuicore.KeyEnter)
	h.key(tuicore.KeyEnter)
	h.settle()

	var open bool
	h.on(func() { open = h.m.modalOpen() })
	if !open {
		t.Fatal("a refused submit closed the modal on a change that did not happen")
	}
	if !strings.Contains(h.screen(), "already taken") {
		t.Fatalf("the refusal is not on screen:\n%s", h.screen())
	}
}

// ExcludeCancel LEAVES ONE BUTTON. For acknowledgements — a modal that asks a
// question keeps a way to say no.
func TestInputModal_ExcludeCancelLeavesOnlyTheAffirmative(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		NewConfirmModal(h.m, "notice", "the store was unlocked").
			WithOkText("CONFIRM").ExcludeCancel().Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })
	h.settle()

	got := h.screen()
	if !strings.Contains(got, "CONFIRM") {
		t.Fatalf("the renamed affirmative is not on screen:\n%s", got)
	}
	if strings.Contains(got, "Cancel") {
		t.Fatalf("ExcludeCancel left the Cancel button on screen:\n%s", got)
	}
}

// A SELECT'S OPTIONS COME OUT IN A STABLE ORDER.
//
// The caller hands over a map, and Go randomises map iteration: without the
// sort the same modal lists the same connections in a different order every
// time it opens, which is a list nobody can learn. Built repeatedly, because
// one unsorted build has a small chance of coming out sorted by luck and eight
// of them do not.
func TestSelectInput_OptionOrderIsStableAndByLabel(t *testing.T) {
	opts := map[int64]string{
		7: "prod-west", 3: "analytics", 11: "staging",
		2: "prod-east", 5: "backup", 9: "dev", 13: "reporting", 4: "archive",
	}
	want := []string{"analytics", "archive", "backup", "dev", "prod-east", "prod-west", "reporting", "staging"}

	for attempt := range 8 {
		var chosen int64
		got := NewSelectInput("connection", opts, &chosen).items()
		if len(got) != len(want) {
			t.Fatalf("attempt %d: %d options, want %d", attempt, len(got), len(want))
		}
		for i := range want {
			if got[i].Label != want[i] {
				t.Fatalf("attempt %d: options are %v, want %v — map iteration order reached the operator",
					attempt, got, want)
			}
		}
	}
}

// ESCAPE ON A MODAL WITH NO CANCEL BUTTON STILL DECLINES.
//
// This is the case the button wiring could not serve. ExcludeCancel leaves no
// Cancel-role button, so widget.Modal has nothing to resolve the dismiss
// request against and closes the dialog directly — and a cancel callback hung
// off that absent button would simply never run.
func TestInputModal_EscapeDeclinesAModalWithNoCancelButton(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	cancelled := make(chan ModalResponse, 1)
	h.on(func() {
		NewConfirmModal(h.m, "notice", "the store was unlocked").
			WithOkText("CONFIRM").
			ExcludeCancel().
			WithCancelFn(func(r ModalResponse) { cancelled <- r }).
			Open()
	})
	h.waitUntil("the modal is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEscape)
	h.waitUntil("the modal closed", func() bool { return !h.m.modalOpen() })

	select {
	case r := <-cancelled:
		if r.Status != StatusCancelled {
			t.Fatalf("status is %v, want cancelled", r.Status)
		}
	default:
		t.Fatal("Escape on a button-less modal ran no cancel callback")
	}
}

// THE QUIT CONFIRMATION IS A FACTORY MODAL, and it keeps the three things it
// had: a bare-letter affirmative, a declining answer that NAMES its outcome,
// and a faded backdrop.
func TestQuit_IsAConfirmModalThatNamesBothOutcomes(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	quit := make(chan struct{}, 1)
	h.on(func() {
		h.m.quit = func() { quit <- struct{}{} }
		h.m.confirmQuit()
	})
	h.waitUntil("the confirmation is open", func() bool { return h.m.modalOpen() })
	h.settle()

	got := h.screen()
	for _, want := range []string{"quit autodb?", "Anything unsaved", "Quit", "Stay"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q is not on the quit confirmation:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Cancel") {
		t.Errorf("the declining answer still reads \"Cancel\"; it should name the outcome")
	}

	// THE BARE LETTER STILL FIRES IT. A confirmation has nothing to type into,
	// so the mnemonic cannot collide with a typed character.
	h.key('y')
	h.waitUntil("the confirmation closed", func() bool { return !h.m.modalOpen() })
	select {
	case <-quit:
	default:
		t.Fatal("`y` did not reach the affirmative")
	}
}

// AND DECLINING DOES NOT QUIT. The cell above passes if every key quits.
func TestQuit_DecliningLeavesTheSessionAlone(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	quit := make(chan struct{}, 1)
	h.on(func() {
		h.m.quit = func() { quit <- struct{}{} }
		h.m.confirmQuit()
	})
	h.waitUntil("the confirmation is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEscape)
	h.waitUntil("the confirmation closed", func() bool { return !h.m.modalOpen() })
	h.settle()
	select {
	case <-quit:
		t.Fatal("Escape on the quit confirmation ended the session")
	default:
	}
}
