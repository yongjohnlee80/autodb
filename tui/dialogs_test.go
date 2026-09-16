package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE NO-DEFAULT RULE IS A MECHANISM, NOT A REQUEST.
//
// Four surfaces must not have a focused, Enter-able affirmative: the allowlist
// consent, and the three prompts where one answer discards the operator's work.
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
				case "openDialogScrimmed", "openFormScrimmed":
					scrimmed[sel.Sel.Name] = append(scrimmed[sel.Sel.Name], filepath.Base(name))
				}
				return true
			})
		}
	}

	if got := len(scrimmed["openDialogScrimmed"]); got != 1 {
		t.Errorf("openDialogScrimmed has %d call sites %v, want exactly 1 (quit)",
			got, scrimmed["openDialogScrimmed"])
	}
	if got := len(scrimmed["openFormScrimmed"]); got != 1 {
		t.Errorf("openFormScrimmed has %d call sites %v, want exactly 1 (login)",
			got, scrimmed["openFormScrimmed"])
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
