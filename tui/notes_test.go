package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *NoteStore {
	t.Helper()
	s, err := NewPersonalNotes(filepath.Join(t.TempDir(), "notes"), "tester")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNotesRoundTripAndModes(t *testing.T) {
	s := testStore(t)
	n, body, err := s.Load(7, "queries")
	if err != nil || body != "" {
		t.Fatalf("new load = (%q, %v)", body, err)
	}
	if err := s.Save(n, "SELECT 1;"); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := filepath.Join(filepath.Join(s.root, s.dir(7)), "queries.sql")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Join(s.root, s.dir(7)))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dst.Mode().Perm())
	}
	names, err := s.List(7)
	if err != nil || len(names) != 1 || names[0] != "queries.sql" {
		t.Fatalf("list = (%v, %v)", names, err)
	}
	_, body2, err := s.Load(7, "queries.sql")
	if err != nil || body2 != "SELECT 1;" {
		t.Fatalf("reload = (%q, %v)", body2, err)
	}
}

func TestNotesNameValidation(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "-flag", "a\x00b"} {
		if _, err := CleanName(bad); err == nil {
			t.Fatalf("CleanName(%q) accepted", bad)
		}
	}
	if got, err := CleanName("My Query-1.v2"); err != nil || got != "My Query-1.v2.sql" {
		t.Fatalf("CleanName = (%q, %v)", got, err)
	}
	if got, _ := CleanName("plain.sql"); got != "plain.sql" {
		t.Fatalf("suffix duplicated: %q", got)
	}
}

func TestNotesConflictDetection(t *testing.T) {
	s := testStore(t)
	n, _, _ := s.Load(1, "shared")
	if err := s.Save(n, "v1"); err != nil {
		t.Fatal(err)
	}

	// A second editor loads, then the first saves again — the second's
	// save must conflict (content identity changed since ITS load).
	n2, _, _ := s.Load(1, "shared")
	if err := s.Save(n, "v2-from-first"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(n2, "v2-from-second"); !errors.Is(err, ErrNoteConflict) {
		t.Fatalf("err = %v, want ErrNoteConflict", err)
	}

	// Same-size edits conflict too (hash, not mtime+size).
	n3, _, _ := s.Load(1, "shared")
	if err := s.Save(n, "v3-from-first"); err != nil { // same length as v2-…? ensure change
		t.Fatal(err)
	}
	if err := s.Save(n3, "anything-else!"); !errors.Is(err, ErrNoteConflict) {
		t.Fatalf("same-size class: err = %v, want ErrNoteConflict", err)
	}

	// NEW-note symmetry (r3): the target appearing since load conflicts.
	fresh, _, _ := s.Load(2, "brand-new")
	other, _, _ := s.Load(2, "brand-new")
	if err := s.Save(other, "raced you"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(fresh, "mine"); !errors.Is(err, ErrNoteConflict) {
		t.Fatalf("new-note appearance: err = %v, want ErrNoteConflict", err)
	}

	// Deleted-underneath conflicts as well.
	n4, _, _ := s.Load(1, "shared")
	if err := s.Delete(1, "shared"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(n4, "ghost"); !errors.Is(err, ErrNoteConflict) {
		t.Fatalf("deleted-underneath: err = %v, want ErrNoteConflict", err)
	}
}

func TestNotesRefuseSymlink(t *testing.T) {
	s := testStore(t)
	if err := os.MkdirAll(filepath.Join(s.root, s.dir(3)), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.sql")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(filepath.Join(s.root, s.dir(3)), "link.sql")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, _, err := s.Load(3, "link"); err == nil {
		t.Fatal("symlink load succeeded")
	}
}
