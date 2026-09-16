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

// goodManifest is a manifest that honestly describes dir.
func goodManifest(t *testing.T, dir string) (string, []Entry) {
	t.Helper()
	_, entries, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := "# digest " + DigestOf(entries) + "\n"
	for _, e := range entries {
		body += e.Sum + " " + e.Mode + " " + e.Path + "\n"
	}
	return body, entries
}

// AN HONEST MANIFEST IS ACCEPTED.
//
// THE POSITIVE CONTROL FOR THE THREE REFUSALS BELOW. Without it they could all
// be satisfied by a parser that refuses everything — a gate that cannot pass
// rather than one that cannot fail, which is the same uselessness wearing the
// opposite coat.
func TestIdentity_AnHonestManifestIsAccepted(t *testing.T) {
	body, entries := goodManifest(t, sampleTree(t))
	got, digest, err := ParseManifest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("an untampered manifest was refused: %v", err)
	}
	if len(got) != len(entries) {
		t.Errorf("parsed %d entries, want %d", len(got), len(entries))
	}
	if digest != DigestOf(entries) {
		t.Error("the parsed digest is not the one the manifest recorded")
	}
}

// A MANIFEST WITH NO DIGEST PINS NOTHING.
//
// Accepted, it would be compared against nothing and the check would report
// success for work it did not do.
func TestIdentity_AManifestWithNoDigestIsRefused(t *testing.T) {
	body, _ := goodManifest(t, sampleTree(t))
	headless := strings.SplitN(body, "\n", 2)[1]

	_, _, err := ParseManifest(strings.NewReader(headless))
	if err == nil {
		t.Fatal("a manifest with no digest was accepted; there would be nothing to check a " +
			"tree against and the gate would pass having compared nothing")
	}
	if !strings.Contains(err.Error(), "carries no digest") {
		t.Errorf("got %v, want it to say the manifest carries no digest", err)
	}
}

// A BODY EDITED UNDER AN UNTOUCHED HEADER IS NOT AUTHORITATIVE.
//
// The manifest is the artifact that pins a tree, so it is also the thing worth
// tampering with. Believing the header without re-deriving it from the entries
// beneath makes the one file nobody checks the one that decides everything.
func TestIdentity_AManifestWithAnEditedBodyIsRefused(t *testing.T) {
	dir := sampleTree(t)
	body, entries := goodManifest(t, dir)
	tampered := strings.Replace(body, entries[0].Sum, strings.Repeat("0", 64), 1)

	_, _, err := ParseManifest(strings.NewReader(tampered))
	if err == nil {
		t.Fatal("a manifest with a body edited under an untouched header was accepted; the " +
			"artifact that pins the tree is the thing most worth tampering with")
	}
	if !strings.Contains(err.Error(), "edited since it was written") {
		t.Errorf("got %v, want it to say the manifest was edited since it was written", err)
	}
}

// AN EMPTY MANIFEST WOULD PASS ANY TREE AT ALL.
func TestIdentity_AnEmptyManifestIsRefused(t *testing.T) {
	body, entries := goodManifest(t, sampleTree(t))
	_ = body
	headerOnly := "# digest " + DigestOf(entries) + "\n"

	_, _, err := ParseManifest(strings.NewReader(headerOnly))
	if err == nil {
		t.Fatal("a manifest listing no files was accepted; read as no-differences it would " +
			"pass any tree at all")
	}
	if !strings.Contains(err.Error(), "pins nothing") {
		t.Errorf("got %v, want it to say the manifest pins nothing", err)
	}
}

// EVIDENCE MAY NOT LIVE INSIDE THE TREE IT DESCRIBES.
//
// A manifest written into its own fingerprint root changes what it records: the
// digest lands in the file, the file lands in the tree, and an exact copy is
// then rejected. That was documented and not enforced, which is the same as not
// fixed — an old command line silently recreates it, and the gate cries wolf on
// correct input.
func TestIdentity_EvidenceInsideTheRootIsRefused(t *testing.T) {
	root := sampleTree(t)
	outside := filepath.Join(filepath.Dir(root), "ledger.manifest")

	for _, tc := range []struct {
		name     string
		path     string
		wantsErr bool
	}{
		{"beside the root", outside, false},
		{"directly inside the root", filepath.Join(root, "local.manifest"), true},
		{"nested inside the root", filepath.Join(root, "core", "exec", "m.manifest"), true},
		{"the root itself", root, true},
		{"reached by a dotted path", filepath.Join(root, "core", "..", "m.manifest"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOutsideRoot(root, tc.path)
			if tc.wantsErr && err == nil {
				t.Errorf("%s was accepted; writing there changes the tree being fingerprinted, "+
					"so an exact copy would be reported as changed", tc.path)
			}
			if !tc.wantsErr && err != nil {
				t.Errorf("%s was refused though it is outside the root: %v", tc.path, err)
			}
			if tc.wantsErr && err != nil && !errors.Is(err, ErrInsideRoot) {
				t.Errorf("got %v, want it to identify as an inside-root refusal", err)
			}
		})
	}
}

// A MANIFEST ASSERTS EXACTLY ONE IDENTITY.
//
// With a later header silently winning, appending a single line to a manifest
// would change what it claims — and the file whose whole purpose is to pin a
// tree would be the easiest thing in the ledger to rewrite.
func TestIdentity_AManifestWithTwoDigestHeadersIsRefused(t *testing.T) {
	dir := sampleTree(t)
	_, entries, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	good := "# digest " + DigestOf(entries) + "\n"
	for _, e := range entries {
		good += e.Sum + " " + e.Mode + " " + e.Path + "\n"
	}

	two := "# digest " + strings.Repeat("a", 64) + "\n" + good
	if _, _, err := ParseManifest(strings.NewReader(two)); err == nil {
		t.Error("a manifest with two digest headers was accepted; appending one line would " +
			"change the identity it asserts")
	}

	// A near-miss must not read as a digest either: it would compare unequal to
	// a real one and produce a mismatch that looks like a changed tree rather
	// than a malformed record.
	for _, bad := range []string{strings.Repeat("A", 64), strings.Repeat("a", 63), "not-a-digest"} {
		body := "# digest " + bad + "\n" + strings.SplitN(good, "\n", 2)[1]
		if _, _, err := ParseManifest(strings.NewReader(body)); err == nil {
			t.Errorf("%q was accepted as a digest", bad)
		}
	}

	// And a declared count that disagrees with the body is refused.
	counted := "# digest " + DigestOf(entries) + "\n# files 999\n" +
		strings.SplitN(good, "\n", 2)[1]
	if _, _, err := ParseManifest(strings.NewReader(counted)); err == nil {
		t.Error("a manifest claiming 999 files was accepted with far fewer; entries could be " +
			"removed without the record noticing")
	}
}
