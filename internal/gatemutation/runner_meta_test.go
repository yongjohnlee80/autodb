package gatemutation_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// META-CELLS FOR THE RUNNER, because the runner is what judges everything else.
//
// A classifier that says RED when it should say INVALID does not produce a
// wrong verdict once; it produces a whole ledger of verdicts that cannot be
// trusted, and the error is invisible precisely because the output looks like
// evidence. So the runner is exercised against trees whose answers are known in
// advance, and the thing asserted is its EXIT CODE and its classification —
// not that it ran.

// runnerAt builds the runner once and returns a way to invoke it.
func runnerAt(t *testing.T) (bin string, repo string) {
	t.Helper()
	repo = repoRootAbs(t)
	bin = filepath.Join(t.TempDir(), "mutate")
	build := exec.Command("go", "build", "-o", bin, "./internal/gatemutation/cmd/mutate")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	return bin, repo
}

func repoRootAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// disposable makes a copy of the repository the runner is allowed to mutate.
func disposable(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	repo := repoRootAbs(t)
	cp := exec.Command("sh", "-c", "tar -cf - --exclude=.git . | tar -x -C "+dst)
	cp.Dir = repo
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("copying the repository: %v\n%s", err, out)
	}
	return dst
}

func runRunner(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running the runner: %v\n%s", err, out)
		}
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// AN UNKNOWN CONTROL NAME IS A USAGE ERROR, NOT A CLEAN RUN.
//
// It used to select zero controls, count zero failures and exit 0 — a run that
// did nothing, reported as success. A typo in a ledger command would have read
// as a green gate.
func TestRunner_AnUnknownControlNameIsRefused(t *testing.T) {
	bin, repo := runnerAt(t)
	out, code := runRunner(t, bin, repo, "-root", disposable(t), "-only", "no-such-control")
	if code != 2 {
		t.Errorf("exit %d for an unknown control, want 2; a name nobody recognises must not "+
			"read as a clean run\n%s", code, out)
	}
}

// A SOURCE TREE IS NOT A MUTATION TARGET.
//
// Mutating one risks leaving somebody's actual work broken if the runner dies
// between the edit and the restore.
func TestRunner_ASourceTreeIsRefused(t *testing.T) {
	bin, repo := runnerAt(t)
	out, code := runRunner(t, bin, repo, "-root", repo, "-only", "identity-excludes-git")
	if code != 2 {
		t.Errorf("exit %d for a root containing .git, want 2\n%s", code, out)
	}
}

// A CELL THAT IS ALREADY RED EARNS INVALID, NOT RED.
//
// THIS IS THE CELL FOR THE DEFECT THAT MOTIVATED THE RUNNER. A control whose
// target was already failing — from unrelated drift, say — used to be scored
// RED, and appeared to prove a guarantee it had never tested. The stock run is
// what tells "the mutation broke this" apart from "this was broken already".
func TestRunner_AnAlreadyFailingCellCannotEarnRed(t *testing.T) {
	bin, repo := runnerAt(t)
	dir := disposable(t)

	// Break the target cell itself, so it fails with or without the mutation.
	victim := filepath.Join(dir, "internal/gateidentity/identity_test.go")
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(body),
		"func TestIdentity_AManifestWithNoDigestIsRefused(t *testing.T) {",
		"func TestIdentity_AManifestWithNoDigestIsRefused(t *testing.T) {\n\tt.Error(\"already failing before any mutation\")",
		1)
	if broken == string(body) {
		t.Fatal("could not break the target cell; this meta-cell is not testing what it claims")
	}
	if err := os.WriteFile(victim, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runRunner(t, bin, repo, "-root", dir, "-only", "identity-requires-a-digest")
	if !strings.Contains(out, "=INVALID") {
		t.Errorf("a control whose cell was already red was not INVALID:\n%s", out)
	}
	if !strings.Contains(out, "not green before mutation") {
		t.Errorf("the runner did not say the cell was red before the mutation:\n%s", out)
	}
	if code == 0 {
		t.Errorf("exit 0 for an unscoreable control; a ledger would record it as passing\n%s", out)
	}
}

// A CONTROL THAT DOES NOT BUILD IS INVALID, NOT GREEN.
//
// A mutated tree that will not compile never reaches the cell, so nothing was
// learned — which is a different thing from a cell that passed.
func TestRunner_AMutationThatBreaksTheBuildIsInvalid(t *testing.T) {
	bin, repo := runnerAt(t)
	dir := disposable(t)

	// Make the package uncompilable in a way the mutation cannot fix.
	victim := filepath.Join(dir, "internal/gateidentity/identity.go")
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, append(body, []byte("\nfunc broken( {}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runRunner(t, bin, repo, "-root", dir, "-only", "identity-verifies-the-body")
	if !strings.Contains(out, "=INVALID") {
		t.Errorf("a control on a tree that does not build was not INVALID:\n%s", out)
	}
	if code == 0 {
		t.Errorf("exit 0 despite an unscoreable control\n%s", out)
	}
}

// THE SOURCE ROOT IS LEFT EXACTLY AS IT WAS FOUND.
//
// One control leaving the tree dirty turns every later verdict into fiction,
// and the failure is silent: the next control simply attacks something that is
// already broken.
func TestRunner_TheTreeIsRestoredAfterAControl(t *testing.T) {
	bin, repo := runnerAt(t)
	dir := disposable(t)

	target := filepath.Join(dir, "internal/gateidentity/identity.go")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	modeBefore, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if _, code := runRunner(t, bin, repo, "-root", dir, "-only", "identity-verifies-the-body"); code != 0 {
		t.Logf("control exited %d; restoration is asserted regardless", code)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the mutated file was not restored byte-for-byte, so every later control " +
			"would attack a tree nobody has described")
	}
	modeAfter, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if modeAfter.Mode() != modeBefore.Mode() {
		t.Errorf("the file's mode changed from %v to %v; a widened mode is a difference the "+
			"next digest reports and nobody can explain", modeBefore.Mode(), modeAfter.Mode())
	}
}
