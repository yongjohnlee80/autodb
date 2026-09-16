package main

import (
	"os"
	"path/filepath"
	"testing"
)

// RESTORATION IS TESTED WHERE IT CAN ACTUALLY FAIL.
//
// An earlier meta-cell drove the whole runner and asserted the target's mode
// afterwards — and it could not fail, because nothing in the runner's write
// path changes a mode: os.WriteFile leaves an existing file's permission alone,
// so the mode it never restored was also the mode nothing had disturbed. The
// cell looked like a guard and was scenery.
//
// The guarantee is real even though the current write path does not provoke it:
// the helper's contract is "put this file back exactly as it was", and a caller
// that ever writes through a path which DOES change the mode would silently
// leave it changed. So the mode is perturbed here deliberately, which is the
// only way to find out whether restore restores it.
func TestRestore_PutsBackBytesAndPermission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	original := []byte("package sample\n")
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}

	// Stand in for a control that both edits the file and changes its mode.
	if err := os.WriteFile(path, []byte("package sample // mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := restore(path, original, 0o755); err != nil {
		t.Fatalf("restore reported failure: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("bytes are %q after restore, want %q", got, original)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode is %v after restore, want 0755 — os.WriteFile's permission argument "+
			"applies only when it CREATES a file, so an existing one keeps whatever mode it "+
			"had and the restore is a claim rather than a fact", st.Mode().Perm())
	}
}

// A RESTORE THAT DID NOT RESTORE IS AN ERROR, NOT A SHRUG.
//
// One control leaving the tree changed turns every later verdict into fiction,
// and the failure is silent: the next control simply attacks something already
// broken.
func TestRestore_ReportsWhenItCannotPutTheFileBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sample.go")

	// No such directory: the write cannot succeed.
	if err := restore(path, []byte("package sample\n"), 0o644); err == nil {
		t.Error("restore reported success for a file it could not write; a failed restore " +
			"must stop the run rather than letting later controls attack a poisoned tree")
	}
}
