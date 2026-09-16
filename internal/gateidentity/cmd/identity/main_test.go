package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE COMMAND BOUNDARY, NOT THE LIBRARY.
//
// The helpers were celled and the CLI was not, which left the part everybody
// actually runs unproven: whether main calls the guard for BOTH flags, refuses
// two authorities, validates its metadata and writes atomically. A library that
// refuses correctly behind a command that never asks it is a gate with a hole
// in exactly the place the hole does not show.
//
// Every row here asserts an EXIT CODE, because that is what a gate script reads:
//   0 — the tree matches
//   1 — the tree differs
//   2 — the command was malformed, ambiguous, or would have written evidence
//       into the thing it describes; nothing is written or compared

func buildTool(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "identity")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the tool: %v\n%s", err, out)
	}
	return bin
}

func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(root, "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"go.mod":         "module sample\n",
		"core/exec.go":   "package core\n",
		"core/policy.go": "package core\n\nfunc p() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func run(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the tool: %v", err)
	}
	return string(out), code
}

// THE HONEST PATH: record outside the tree, compare a faithful copy, exit 0.
func TestCLI_AnHonestComparisonSucceeds(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	ledger := filepath.Join(filepath.Dir(root), "ledger.manifest")

	if out, code := run(t, bin, "-dir", root, "-manifest", ledger); code != 0 {
		t.Fatalf("recording exited %d: %s", code, out)
	}
	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("no manifest was written: %v", err)
	}

	copyDir(t, root, filepath.Join(filepath.Dir(root), "copy"))
	out, code := run(t, bin, "-dir", filepath.Join(filepath.Dir(root), "copy"), "-against", ledger)
	if code != 0 {
		t.Errorf("an exact copy exited %d, want 0: %s", code, out)
	}
}

// A CHANGED BYTE EXITS 1 AND NAMES THE FILE.
func TestCLI_AChangedTreeExitsOneAndNamesTheFile(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	ledger := filepath.Join(filepath.Dir(root), "ledger.manifest")
	if _, code := run(t, bin, "-dir", root, "-manifest", ledger); code != 0 {
		t.Fatal("recording failed")
	}

	copied := filepath.Join(filepath.Dir(root), "copy")
	copyDir(t, root, copied)
	if err := os.WriteFile(filepath.Join(copied, "core/policy.go"),
		[]byte("package core\n\nfunc p() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, bin, "-dir", copied, "-against", ledger)
	if code != 1 {
		t.Errorf("a changed tree exited %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "core/policy.go") {
		t.Errorf("the mismatch did not name the file that differs: %s", out)
	}
}

// RECORDING INTO THE ROOT IS REFUSED BEFORE ANYTHING IS WRITTEN.
//
// ISOLATED, NOT A SUBTEST. Two routes into the same guard sharing one cell
// means either can supply the failure, so a control that bypasses one of them
// still reddens and looks proven.
func TestCLI_RecordingIntoTheRootIsRefused(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	inside := filepath.Join(root, "local.manifest")

	out, code := run(t, bin, "-dir", root, "-manifest", inside)
	if code != 2 {
		t.Errorf("exit %d for a manifest inside the root, want 2: %s", code, out)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("the manifest was written anyway; the refusal must come before the write, " +
			"or the tree is already changed by the time it is refused")
	}
}

// CHECKING AGAINST A MANIFEST IN THE ROOT IS REFUSED.
func TestCLI_CheckingAgainstAManifestInTheRootIsRefused(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)

	// A VALID manifest, deliberately. An unreadable one also exits 2, so the
	// cell would pass for the wrong reason and could not tell whether the
	// containment guard ran at all — which is exactly what it did at first.
	outside := filepath.Join(filepath.Dir(root), "ledger.manifest")
	if _, code := run(t, bin, "-dir", root, "-manifest", outside); code != 0 {
		t.Fatal("recording an honest manifest failed")
	}
	inside := filepath.Join(root, "local.manifest")
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, bin, "-dir", root, "-against", inside)
	if code != 2 {
		t.Errorf("exit %d for an against-manifest inside the root, want 2: %s", code, out)
	}
}

// TWO AUTHORITIES ARE REFUSED.
func TestCLI_ExpectAndAgainstTogetherAreRefused(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	ledger := filepath.Join(filepath.Dir(root), "ledger.manifest")
	if _, code := run(t, bin, "-dir", root, "-manifest", ledger); code != 0 {
		t.Fatal("recording failed")
	}
	out, code := run(t, bin, "-dir", root, "-against", ledger, "-expect", strings.Repeat("a", 64))
	if code != 2 {
		t.Errorf("exit %d, want 2: a manifest and a flag claiming different identities must "+
			"not both be accepted, or the check passes while the record describes "+
			"something else: %s", code, out)
	}
}

// METADATA THAT COULD FORGE A HEADER IS REFUSED.
func TestCLI_MalformedMetadataIsRefused(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	ledger := filepath.Join(filepath.Dir(root), "ledger.manifest")

	for _, tc := range []struct{ name, flag, val string }{
		{"a short head", "-head", "abc123"},
		{"a note with a newline", "-note", "ok\n# digest " + strings.Repeat("b", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(t, bin, "-dir", root, "-manifest", ledger, tc.flag, tc.val)
			if code != 2 {
				t.Errorf("exit %d, want 2: %s", code, out)
			}
		})
	}
}

// RECORDING REPLACES DETERMINISTICALLY AND LEAVES NO DEBRIS.
//
// NARROWED TO WHAT IT ACTUALLY PROVES. The title once claimed it showed an
// interrupted write preserves the previous evidence; it shows no such thing,
// because nothing here interrupts anything. What it does establish is that two
// recordings of an unchanged tree agree and that no partial file is left beside
// the manifest. The crash-durability claim needs an injected pre-rename failure
// and is not made here.
func TestCLI_RecordingReplacesAtomically(t *testing.T) {
	bin := buildTool(t)
	root := tree(t)
	ledger := filepath.Join(filepath.Dir(root), "ledger.manifest")

	if _, code := run(t, bin, "-dir", root, "-manifest", ledger); code != 0 {
		t.Fatal("first recording failed")
	}
	first, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := run(t, bin, "-dir", root, "-manifest", ledger); code != 0 {
		t.Fatal("second recording failed")
	}
	second, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two recordings of an unchanged tree produced different manifests")
	}

	// No temp files left beside it: a directory littered with half-written
	// evidence is its own hazard.
	entries, err := os.ReadDir(filepath.Dir(ledger))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a temporary manifest was left behind: %s", e.Name())
		}
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "mkdir -p "+dst+" && tar -cf - -C "+src+" . | tar -x -C "+dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copying: %v\n%s", err, out)
	}
}
