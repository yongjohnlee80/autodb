package gateidentity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleTree builds a small tree whose content the cell controls.
func sampleTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	must("go.mod", "module sample\n", 0o644)
	must("core/exec/scheduler.go", "package exec\n\nfunc serve() {}\n", 0o644)
	must("scripts/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	return dir
}

// THE SAME TREE ALWAYS GIVES THE SAME ANSWER.
//
// A fingerprint that varied between runs would be worse than none: every
// comparison would fail, everyone would learn to ignore it, and the one real
// mismatch would be ignored with the rest.
func TestIdentity_TheDigestIsStableForUnchangedContent(t *testing.T) {
	dir := sampleTree(t)
	first, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same tree fingerprinted as %s and then %s", first, second)
	}
	if first == "" {
		t.Error("the digest is empty")
	}
}

// A CHANGED BYTE IS REJECTED, AND THE FILE IS NAMED.
//
// THIS IS THE NEGATIVE CONTROL THE GATE DID NOT HAVE. A run once reported
// identity=0 with nothing in its log but a git command that had failed by
// design, so the step could not have detected a mismatched tree at all: it
// passed by having no opinion, and every green result beneath it was attributed
// to a tree nobody had checked. The commonest real cause is a mutation left
// un-reverted, which is why the differing path is named rather than merely
// counted.
func TestIdentity_AChangedByteIsRejectedAndNamed(t *testing.T) {
	dir := sampleTree(t)
	expected, entries, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, expected, entries); err != nil {
		t.Fatalf("an unchanged tree was rejected: %v", err)
	}

	// Exactly the shape of a mutation that was not put back.
	victim := filepath.Join(dir, "core/exec/scheduler.go")
	if err := os.WriteFile(victim, []byte("package exec\n\nfunc serve() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = Verify(dir, expected, entries)
	if err == nil {
		t.Fatal("a tree with a changed file matched its old digest, so the identity gate " +
			"cannot fail and every result beneath it is attributed to a tree nobody checked")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("got %v, want a mismatch", err)
	}
	if !strings.Contains(err.Error(), "core/exec/scheduler.go") {
		t.Errorf("the mismatch did not name the file that differs: %v", err)
	}
	if !strings.Contains(err.Error(), "changed:") {
		t.Errorf("the mismatch did not say the file was changed rather than added or "+
			"removed: %v", err)
	}
}

// ADDING AND REMOVING FILES IS ALSO A DIFFERENCE.
//
// A digest over content alone would miss a file that appeared or vanished,
// which is exactly what a half-applied patch looks like.
func TestIdentity_AddedAndRemovedFilesAreCaught(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, dir string)
		wants string
	}{
		{
			name: "a file appears",
			spoil: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "extra.go"), []byte("package x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wants: "added:",
		},
		{
			name: "a file vanishes",
			spoil: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "go.mod")); err != nil {
					t.Fatal(err)
				}
			},
			wants: "removed:",
		},
		{
			name: "a file stops being executable",
			spoil: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "scripts/run.sh"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wants: "mode:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := sampleTree(t)
			expected, entries, err := Digest(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.spoil(t, dir)
			err = Verify(dir, expected, entries)
			if err == nil {
				t.Fatalf("the identity gate accepted a tree where %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the mismatch did not report %q: %v", tc.wants, err)
			}
		})
	}
}

// THE EXCLUDED DIRECTORIES ARE EXCLUDED.
//
// The copy under test deliberately has no .git, which is the whole reason a
// git command cannot be the identity check. Counting it would make every
// comparison between a source worktree and its copy fail for a reason that has
// nothing to do with the code.
func TestIdentity_TheGitDirectoryIsNotPartOfTheFingerprint(t *testing.T) {
	dir := sampleTree(t)
	before, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git/HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("the fingerprint changed when a .git directory appeared, so a worktree and " +
			"its .git-excluded copy could never be compared")
	}
}

// A WORKTREE'S .git IS A FILE, AND IT IS EXCLUDED TOO.
//
// THIS CELL EXISTS BECAUSE THE FIRST VERSION SKIPPED DIRECTORIES ONLY. Every
// worktree in this repository keeps a `.git` FILE holding a gitdir pointer, so
// the source side fingerprinted it, the .git-excluded copy did not, and every
// honest copy was rejected. A gate that cries wolf on correct input is worse
// than no gate: it teaches everybody to pass over the one time it is right.
func TestIdentity_AWorktreeGitFileIsNotPartOfTheFingerprint(t *testing.T) {
	dir := sampleTree(t)
	before, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what a git worktree has where a clone has a directory.
	if err := os.WriteFile(filepath.Join(dir, ".git"),
		[]byte("gitdir: /somewhere/.git/worktrees/wt-example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("the fingerprint changed when a worktree's .git FILE appeared, so every " +
			"honest copy of a worktree would be rejected and the gate would cry wolf")
	}
}

// A MANIFEST IS CHECKED AGAINST ITSELF BEFORE IT IS BELIEVED.
//
// THESE ARE FAIL-OPEN CONTROLS. The manifest is the artifact that pins a tree,
// so it is also the thing worth tampering with: a body edited under an
// untouched header would have been read as authoritative, and an empty one read
// as "no differences". Each row below is a manifest that must be REFUSED rather
// than quietly believed.
func TestIdentity_AManifestThatCannotBeTrustedIsRefused(t *testing.T) {
	dir := sampleTree(t)
	_, entries, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	good := "# digest " + DigestOf(entries) + "\n"
	for _, e := range entries {
		good += e.Sum + " " + e.Mode + " " + e.Path + "\n"
	}

	for _, tc := range []struct {
		name  string
		body  string
		wants string
	}{
		{
			name:  "a body edited under an untouched header",
			body:  strings.Replace(good, entries[0].Sum, strings.Repeat("0", 64), 1),
			wants: "edited since it was written",
		},
		{
			name:  "no digest header at all",
			body:  strings.SplitN(good, "\n", 2)[1],
			wants: "carries no digest",
		},
		{
			name:  "a header with no entries",
			body:  "# digest " + DigestOf(entries) + "\n",
			wants: "pins nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseManifest(strings.NewReader(tc.body))
			if err == nil {
				t.Fatalf("a manifest with %s was accepted; the artifact that pins the tree "+
					"is the thing most worth tampering with", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("got %v, want it to say %q", err, tc.wants)
			}
		})
	}

	// And the honest one is still accepted, or the controls above prove nothing.
	if _, _, err := ParseManifest(strings.NewReader(good)); err != nil {
		t.Errorf("an untampered manifest was refused: %v", err)
	}
}
