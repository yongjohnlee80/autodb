package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE NO-DEFAULT RULE IS A MECHANISM, NOT A REQUEST.
//
// SIX surfaces must not have a focused, Enter-able affirmative: note deletion
// and token revocation (irreversible), the allowlist consent, and the three
// prompts where one answer discards the operator's work.
// A comment asking the next contributor not to add one is not a guard — this
// is, and this cell is what says so.
func TestOpenDialogNoDefault_RefusesADefaultAnswer(t *testing.T) {
	m := &Model{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a ButtonRoleDefault answer was accepted by openDialogNoDefault")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "ButtonRoleDefault") {
			t.Errorf("panic %q does not say what was wrong", msg)
		}
	}()
	m.openDialogNoDefault("q?", "", affirm('y', "Yes", func() {}))
}

// The declining and alternative answers carry no default, so the same call is
// well-formed without one — otherwise the cell above would pass against a
// helper that refuses everything.
func TestOpenDialogNoDefault_AcceptsAnswersWithoutOne(t *testing.T) {
	for _, a := range []dialogAnswer{
		decline('n', "Cancel"),
		alternative('y', "Do it", func() {}),
	} {
		if a.role == widget.ButtonRoleDefault {
			t.Errorf("%q is built with ButtonRoleDefault; the helper would refuse it", a.label)
		}
	}
}

// THE SCRIM IS ON EXACTLY TWO SURFACES: login and quit.
//
// Read from the SOURCE rather than from a rendered frame, because the claim is
// about every call site at once and a screen shows one. Modal defaults the
// scrim to TRUE — the inverse of Float — so a dialog added without thinking
// about it fades the backdrop by doing nothing, and the count is what catches
// a third one appearing.
//
// IT COUNTS EVERY SPELLING, NOT ONE HELPER EACH. It used to require exactly
// one openDialogScrimmed and exactly one openFormScrimmed, which made the
// guard a statement about two FUNCTIONS rather than about the scrim: moving
// quit onto the modal factory, where the scrim is asked for with Scrimmed(),
// would have left the guard counting a helper nobody calls and seeing nothing
// at the surface that had actually changed. The union is the property.
func TestScrim_IsPassedByExactlyTwoCallSites(t *testing.T) {
	pkg := parsePackage(t)
	scrimmed := map[string][]string{}
	for _, p := range pkg {
		for name, file := range p.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "openDialogScrimmed", "openFormScrimmed", "Scrimmed":
					scrimmed[sel.Sel.Name] = append(scrimmed[sel.Sel.Name], filepath.Base(name))
				}
				return true
			})
			// AND THE STRUCT LITERAL SPELLING. Login asks for the scrim as a
			// formOpts field rather than through a helper, and a guard that
			// only knew the helper names stopped seeing it the moment that
			// changed -- reporting one scrimmed surface where there were two,
			// which is the direction that does not fail loudly.
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "formOpts" {
					return true
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, ok := kv.Key.(*ast.Ident)
					if !ok || k.Name != "scrim" {
						continue
					}
					if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
						scrimmed["formOpts{scrim:true}"] =
							append(scrimmed["formOpts{scrim:true}"], filepath.Base(name))
					}
				}
				return true
			})
		}
	}

	total := 0
	for _, sites := range scrimmed {
		total += len(sites)
	}
	if total != 2 {
		t.Errorf("the scrim is requested at %d call sites %v, want exactly 2 (login and quit)",
			total, scrimmed)
	}
	// AND BOTH ARE STILL THE NAMED ONES. A count of two is also what two NEW
	// scrimmed surfaces would give after login and quit lost theirs, so the
	// FILES are asserted too: login and quit both live in ui.go, and a scrim
	// appearing anywhere else is the thing this guard exists to catch.
	for fn, sites := range scrimmed {
		for _, f := range sites {
			if f != "ui.go" {
				t.Errorf("%s asks for the scrim in %s; only login and quit (ui.go) may",
					fn, f)
			}
		}
	}
}

// openLeader SURVIVES, and that is the point of D11.
//
// Converting the HELPER — rather than the named surfaces that used it — would
// have turned the leader menu into a dialog, which it is not. Three callers
// remain, and all three are MENUS: the Space menu, the keyslot action list, and
// the explorer's "add to this workspace" chooser.
//
// THE COUNT IS THE ASSERTION, deliberately. An earlier version of the Phase 3
// inventory enumerated the float helpers and read the result as exhaustive; it
// was not, and these two explorer call sites were missing from it. A cell that
// checks a LIST I wrote would have agreed with me. A cell that counts what is
// actually there did not.
func TestOpenLeader_StillServesTheMenusAndNothingElse(t *testing.T) {
	pkg := parsePackage(t)
	var callers []string
	for _, p := range pkg {
		for name, file := range p.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "openLeader" {
					callers = append(callers, filepath.Base(name))
				}
				return true
			})
		}
	}
	if len(callers) != 3 {
		t.Errorf("openLeader has %d call sites %v, want 3 — the Space menu, the "+
			"keyslot action list, and the explorer's add chooser. A CONFIRMATION "+
			"belongs in dialogs.go; a menu of different operations belongs here",
			len(callers), callers)
	}
}

// parsePackage reads this package's own source, excluding tests. Resolved from
// this file's location rather than the process directory, for the reason
// recorded on sourcePath.
func parsePackage(t *testing.T) map[string]*ast.Package {
	t.Helper()
	pkg, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(sourcePath("dialogs.go")),
		func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	return pkg
}

// EVERY ANSWER CLOSES THE DIALOG, including the ones that do something.
//
// A Modal dismisses itself for Escape and for nothing else: an activated button
// runs its callback and leaves the card on screen. That is how a confirmation
// comes to accept the same answer twice — press y, the thing happens, the
// dialog is still there, press y again. Both halves are asserted, because the
// declining answer closing is not evidence that the affirmative does.
func TestDialog_EveryAnswerDismissesIt(t *testing.T) {
	for _, tc := range []struct {
		what    string
		press   rune
		wantRan bool
	}{
		{"the affirmative", 'y', true},
		{"the declining answer", 'n', false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h := startBar(t, meta.RoleAdmin)
			ran := false
			h.on(func() {
				h.m.openDialog("proceed?", "This is the question.",
					affirm('y', "Yes", func() { ran = true }),
					decline('n', "No"))
			})
			h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

			h.key(tc.press)
			h.waitUntil("the dialog closed on "+string(tc.press), func() bool {
				return !h.m.modalOpen()
			})

			var got bool
			h.on(func() { got = ran })
			if got != tc.wantRan {
				t.Errorf("%s ran the action = %v, want %v", tc.what, got, tc.wantRan)
			}
		})
	}
}

// Escape closes a dialog and runs nothing, which is what makes "no default"
// mean "the safe answer is the one you get by walking away".
func TestDialog_EscapeRunsNothing(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	ran := false
	h.on(func() {
		h.m.openDialogNoDefault("proceed?", "",
			alternative('y', "Do it", func() { ran = true }),
			decline('n', "Cancel"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEscape)
	h.waitUntil("the dialog closed on Escape", func() bool { return !h.m.modalOpen() })

	var got bool
	h.on(func() { got = ran })
	if got {
		t.Error("Escape ran the affirmative action")
	}
}

// BARE ENTER MUST NOT FIRE AN IRREVERSIBLE ANSWER.
//
// THIS IS THE CELL THAT WAS MISSING, and its absence is why the defect shipped.
// The first version of openDialogNoDefault refused a ButtonRoleDefault answer,
// and a cell asserted that the refusal fired. Both worked. But Modal seeds focus
// on the default button IF THERE IS ONE and otherwise on the FIRST ENABLED
// BUTTON — so a dialog with no default still had its first answer under the
// cursor, and Delete listed the irreversible one first. The guard measured the
// ROLE; the affordance is decided by ORDER; the cell measured the guard.
//
// A reviewer pressing Enter on VM43 found it. This is that keypress.
func TestDialogNoDefault_BareEnterDoesNotRunTheIrreversibleAnswer(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	ran := false
	h.on(func() {
		// Declared in the dangerous order on purpose: the affirmative FIRST,
		// which is exactly how the delete and revoke call sites read.
		h.m.openDialogNoDefault("delete it?", "There is no undo.",
			alternative('y', "Delete", func() { ran = true }),
			decline('n', "Cancel"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEnter)
	h.settle()

	var fired bool
	h.on(func() { fired = ran })
	if fired {
		t.Fatal("bare Enter on an untouched dialog ran the irreversible answer; " +
			"Modal focuses the first enabled button when nothing is default")
	}
}

// POSITIVE CONTROL 1: the mnemonic still runs it. Without this, the cell above
// would pass against a dialog whose affirmative is simply broken.
func TestDialogNoDefault_TheMnemonicStillRunsTheAnswer(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	ran := false
	h.on(func() {
		h.m.openDialogNoDefault("delete it?", "There is no undo.",
			alternative('y', "Delete", func() { ran = true }),
			decline('n', "Cancel"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key('y')
	h.waitUntil("the dialog closed", func() bool { return !h.m.modalOpen() })

	var fired bool
	h.on(func() { fired = ran })
	if !fired {
		t.Error("the mnemonic did not run the answer; the affirmative is unreachable")
	}
}

// POSITIVE CONTROL 2: Enter DOES fire the affirmative on a dialog that declares
// a default. So the first cell is about the no-default rule rather than about
// Enter never activating anything.
func TestDialog_WithADefaultBareEnterDoesRunIt(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	ran := false
	h.on(func() {
		h.m.openDialog("proceed?", "",
			affirm('y', "Yes", func() { ran = true }),
			decline('n', "No"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEnter)
	h.waitUntil("the dialog closed", func() bool { return !h.m.modalOpen() })

	var fired bool
	h.on(func() { fired = ran })
	if !fired {
		t.Error("Enter did not activate the default answer")
	}
}

// A no-default dialog with no declining answer is refused at construction,
// because Modal would then focus whatever came first.
func TestDialogNoDefault_RequiresADecliningAnswer(t *testing.T) {
	m := &Model{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a dialog with no declining answer was accepted")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "declining") {
			t.Errorf("panic %q does not say what is missing", msg)
		}
	}()
	m.openDialogNoDefault("q?", "", alternative('y', "Do it", func() {}))
}

// `q` PARITY, which the conversion took away silently.
//
// These surfaces were leaderMenu confirmations and honoured `q` alongside
// Escape. A Modal traps focus and swallows it, so becoming a dialog removed the
// key while dismissKey's comment went on promising it — nothing failed, and a
// reviewer pressing the key on VM43 is what found it. golib v0.5.25 added
// WithModalDismissKeys; these cells are what stop it going missing again.
func TestDialog_QDismissesAConfirmation(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	ran := false
	h.on(func() {
		h.m.openDialog("proceed?", "This is the question.",
			affirm('y', "Yes", func() { ran = true }),
			decline('n', "No"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key('q')
	h.waitUntil("`q` closed the confirmation", func() bool { return !h.m.modalOpen() })

	var fired bool
	h.on(func() { fired = ran })
	if fired {
		t.Error("`q` ran the affirmative; it must dismiss, not choose")
	}
}

// AND ESCAPE STILL DOES TOO, so the cell above is about `q` being restored
// rather than about dismissal in general.
func TestDialog_EscapeDismissesAConfirmation(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openDialog("proceed?", "", affirm('y', "Yes", func() {}), decline('n', "No"))
	})
	h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

	h.key(tuicore.KeyEscape)
	h.waitUntil("Escape closed the confirmation", func() bool { return !h.m.modalOpen() })
}

// A FORM DOES NOT TAKE `q`, and this is the exclusion dismissKey calls
// permanent. `q` is a typed character in a CIDR, a note name, a passphrase or a
// PAT label — a form that closed on it could not accept one.
func TestForm_QIsTypedNotDismissed(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() {
		h.m.openForm("name it", []formField{field("name")},
			func(formValues) (bool, string) { return true, "" })
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })

	h.key('q')
	h.settle()

	var open bool
	h.on(func() { open = h.m.modalOpen() })
	if !open {
		t.Fatal("`q` closed a FORM; it is a character there, and the exclusion is permanent")
	}
}

// DISMISSAL PROVENANCE: the reason names WHICH ANSWER ended the dialog.
//
// It used to be hard-coded to Accept for every button, so a declining answer
// reported acceptance — and Escape and `q` did too, because both resolve through
// the Cancel-role button whose callback dismissed first, leaving golib's own
// DismissCancel a no-op on an already-closed dialog. Anything listening for
// provenance was told every exit was a yes.
//
// A table, because the affirmative case passing tells you nothing about the
// three that were wrong.
func TestDialog_DismissalReasonFollowsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		what string
		act  func(h *barHarness)
		want widget.DismissReason
	}{
		{"the affirmative", func(h *barHarness) { h.key('y') }, widget.DismissAccept},
		{"the declining answer", func(h *barHarness) { h.key('n') }, widget.DismissCancel},
		{"Escape", func(h *barHarness) { h.key(tuicore.KeyEscape) }, widget.DismissCancel},
		{"the dismiss key", func(h *barHarness) { h.key('q') }, widget.DismissCancel},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h := startBar(t, meta.RoleAdmin)
			var got []widget.DismissReason
			var mu sync.Mutex
			h.on(func() {
				tuicore.Subscribe(h.app.Bus(), func(ev widget.OverlayDismissedEvent) {
					mu.Lock()
					got = append(got, ev.Reason)
					mu.Unlock()
				})
				h.m.openDialog("proceed?", "",
					affirm('y', "Yes", func() {}),
					decline('n', "No"))
			})
			h.waitUntil("the dialog is open", func() bool { return h.m.modalOpen() })

			tc.act(h)
			h.waitUntil("the dialog closed", func() bool { return !h.m.modalOpen() })
			h.settle()

			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("published %d dismissals, want exactly 1: %v", len(got), got)
			}
			if got[0] != tc.want {
				t.Errorf("%s reported %v, want %v", tc.what, got[0], tc.want)
			}
		})
	}
}
