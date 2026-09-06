package exec

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release returns the slot: after it runs, the session can be claimed again.
//
// This is the behavioural half, and it is the half that can be observed without
// adding a seam to production code for a test's benefit. The ORDER of finish()
// and finishClosing is asserted structurally instead — see
// TestClaimSessionReleasesBeforeItCloses and read its comment before trusting
// this pair as full coverage.
func TestClaimSessionReleaseReturnsTheSlot(t *testing.T) {
	e := &Engine{}
	s := &session{id: "claim", userID: 1, connID: 1}

	release, flag, err := e.claimSession(context.Background(), s)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if flag == nil || *flag {
		t.Fatalf("the flag starts at %v, want false — a claim must not decide to close", flag)
	}
	if _, _, err := e.claimSession(context.Background(), s); err == nil {
		t.Fatal("a second claim succeeded while the first was held; one-in-flight is " +
			"what stops a caller queueing behind work it cannot see")
	}
	release()
	release2, _, err := e.claimSession(context.Background(), s)
	if err != nil {
		t.Fatalf("claiming after release: %v — the release did not return the slot", err)
	}
	release2()
}

// A refused claim must hand back nothing to defer.
func TestClaimSessionRefusalReturnsNoRelease(t *testing.T) {
	e := &Engine{}
	s := &session{id: "claim", userID: 1, connID: 1}
	release, _, err := e.claimSession(context.Background(), s)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer release()

	release2, flag2, err2 := e.claimSession(context.Background(), s)
	if err2 == nil {
		t.Fatal("the second claim was not refused")
	}
	if release2 != nil || flag2 != nil {
		t.Fatal("a refused claim returned a release or a flag; a caller that defers " +
			"what it was refused would return a slot it never took")
	}
}

// finish() must precede finishClosing, and finishClosing must run under a
// context the caller's cancellation cannot reach.
//
// ASSERTED STRUCTURALLY, AND I WANT THE LIMIT STATED RATHER THAN IMPLIED. The
// behavioural version needs to observe finishClosing running, which means a
// hook in production code added for a test — and the ordering it would prove is
// only wrong under a close racing a statement, which no unit cell reaches. So
// this reads claimSession's own source: inside the returned closure, the
// s.finish() call must come before the e.finishClosing call, and the context
// passed to finishClosing must be context.WithoutCancel.
//
// A source-order assertion is weaker than a behavioural one. It is not weaker
// than what was there before, which was three copies and a comment.
func TestClaimSessionReleasesBeforeItCloses(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "session_engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "claimSession" {
			body = fd
		}
	}
	if body == nil {
		t.Fatal("claimSession not found in session_engine.go; this cell is asserting " +
			"about a function that is not there")
	}

	finishPos, closePos := token.NoPos, token.NoPos
	withoutCancel := false
	ast.Inspect(body.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "finish":
			if finishPos == token.NoPos {
				finishPos = call.Pos()
			}
		case "finishClosing":
			closePos = call.Pos()
			for _, a := range call.Args {
				inner, ok := a.(*ast.CallExpr)
				if !ok {
					continue
				}
				if s2, ok := inner.Fun.(*ast.SelectorExpr); ok && s2.Sel.Name == "WithoutCancel" {
					withoutCancel = true
				}
			}
		}
		return true
	})

	if finishPos == token.NoPos || closePos == token.NoPos {
		t.Fatalf("found finish=%v finishClosing=%v in claimSession; both must be there "+
			"or the ordering below is asserted about nothing", finishPos.IsValid(), closePos.IsValid())
	}
	if finishPos > closePos {
		t.Fatalf("finishClosing (line %d) runs BEFORE finish (line %d). finishClosing "+
			"tears down a session that must no longer be claimed; running it first "+
			"tears down one the engine still believes is executing",
			fset.Position(closePos).Line, fset.Position(finishPos).Line)
	}
	if !withoutCancel {
		t.Fatal("finishClosing is not called with context.WithoutCancel. A close that " +
			"begins as the statement ends would inherit the caller's cancellation and " +
			"be abandoned halfway")
	}
}

// Nobody hand-rolls the preamble any more.
//
// The three copies were identical, so the guard is not about style: a fourth
// copy that reversed the two calls, or dropped context.WithoutCancel, would
// look exactly like the others and fail only under a close racing a statement.
func TestNoHandRolledSessionClaim(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked, implHasBegin := 0, false
	wireExtExemptionUsed := false
	var offenders []string

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		checked++
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "begin" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "s" {
					return true
				}
				if fd.Name.Name == "claimSession" {
					implHasBegin = true
					return true
				}
				// wireExtEntry is EXEMPT BY NAME, and the exemption is a
				// GUARDED CLAIM rather than a bypass — see
				// TestOnlyTheTwoAdmissionPathsEnforceTransactionAuthority, which
				// fails the day its premise stops being true.
				//
				// Its release is `func() { s.finish() }` — no closeAfterRelease,
				// no finishClosing. TRACED, not assumed: the close comes from
				// transferDemotionClose, which is reached only when
				// enforceTransactionAuthority returns an error, and that function
				// has exactly two non-test callers — session_engine.go (the token
				// path) and wire_execute.go (inside wireAdmit). The extended path
				// calls resolveUnitPolicy three times and enforceTransactionAuthority
				// never, so the demotion close cannot arise there.
				//
				// So the simpler release is CORRECT, not an oversight, and folding
				// it into claimSession would ADD a close path the extended protocol
				// does not have. Exempted by NAME rather than by shape so a future
				// hand-rolled claim cannot inherit the exemption by looking similar.
				if fd.Name.Name == "wireExtEntry" {
					wireExtExemptionUsed = true
					return true
				}
				offenders = append(offenders,
					filepath.Base(path)+" in "+fd.Name.Name)
				return true
			})
		}
	}

	if checked < 10 {
		t.Fatalf("parsed only %d non-test file(s); a clean result would mean nothing", checked)
	}
	// "Nobody but claimSession calls s.begin()" is satisfied by NOBODY calling
	// it, which would mean the claim had been deleted rather than shared.
	if !implHasBegin {
		t.Fatal("claimSession does not call s.begin(); the assertion below then holds " +
			"because the session claim has ceased to exist, which is the vacuous pass")
	}
	// AND THE EXEMPTION MUST STILL BE LIVE.
	//
	// A named exemption for a site that no longer exists is not dormant: it
	// stands ready to excuse whatever is written under that name next. If
	// wireExtEntry stops claiming a session itself, this guard silently
	// pre-authorises whatever a future wireExtEntry does with s.begin().
	//
	// The fourth part of the exemption mechanism, after: exempt by name, state
	// the reason, guard the premise. Adopted across every named-exemption guard
	// in the tree after a sibling guard's first exemption list was found to
	// carry four entries naming sites that had none — every one of them a path
	// guessed at rather than opened.
	if !wireExtExemptionUsed {
		t.Fatal("wireExtEntry does not call s.begin(), so its exemption is dead. " +
			"An exemption nothing uses does not lapse — it waits, and excuses the " +
			"next thing written there.")
	}
	if len(offenders) > 0 {
		t.Fatalf("s.begin() is called outside claimSession:\n  %s\n\n"+
			"The preamble is claim + flag + a release that finishes before it closes, "+
			"and a copy that reorders those two or drops context.WithoutCancel looks "+
			"identical and fails only under a close racing a statement.",
			strings.Join(offenders, "\n  "))
	}
}

// The exemption above rests on a reachability claim. This is what makes it a
// claim the suite holds rather than a comment the suite ignores.
//
// wireExtEntry may claim a session without a close path ONLY because the
// extended protocol cannot reach a demotion close. That is true because
// enforceTransactionAuthority — the sole route to transferDemotionClose — is
// called from exactly two places, neither of them in wire_extended*.go. The day
// someone calls it from the extended path, wireExtEntry's release becomes a
// leak of a session the engine believes is closing, and this cell fails first.
//
// (lector, on the finding: "keep the named exemption only if the guard records
// the reachability question and adds a dedicated assertion target; otherwise it
// risks becoming a forgotten permanent bypass.")
func TestOnlyTheTwoAdmissionPathsEnforceTransactionAuthority(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"session_engine.go":  true, // the token path
		"wire_execute.go":    true, // wireAdmit, the simple/wire path
		"session_timeout.go": true, // the declaration itself
	}
	callers := map[string]bool{}
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
				sel.Sel.Name == "enforceTransactionAuthority" {
				callers[filepath.Base(path)] = true
			}
			return true
		})
	}
	if checked < 10 {
		t.Fatalf("parsed only %d non-test file(s); a clean result would mean nothing", checked)
	}
	// Nobody calling it would satisfy the loop below vacuously, and would also
	// mean the demotion path had been deleted rather than left alone.
	if len(callers) == 0 {
		t.Fatal("enforceTransactionAuthority is called from nowhere; either it was " +
			"removed, or this cell is no longer finding calls — both make the " +
			"wireExtEntry exemption unfounded")
	}
	for f := range callers {
		if !allowed[f] {
			t.Errorf("enforceTransactionAuthority is called from %s. wireExtEntry is "+
				"exempt from the session-claim guard BECAUSE the extended path cannot "+
				"reach a demotion close; a call from here breaks that premise, and its "+
				"release (s.finish() alone) then leaks a session the engine believes "+
				"is closing", f)
		}
	}
}
