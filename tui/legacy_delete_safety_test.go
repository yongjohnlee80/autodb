package tui

// Deleting a legacy note must not escape the base — lector's reproduced probe.
//
// A path-based os.Remove addresses the name twice: once when you check it, once
// when the kernel resolves it. Replacing `<base>/ws-N` with a symlink between
// those two moments made the unlink remove a file in an entirely different
// directory.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyDeleteRefusesASymlinkedWorkspaceDir(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.sql")
	if err := os.WriteFile(victim, []byte("not yours to delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	// `<base>/ws-1` IS a symlink to a directory outside the tree.
	if err := os.Symlink(outside, filepath.Join(base, "ws-1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := OpenLegacyNotes(base).Delete(1, "victim.sql")

	if _, serr := os.Stat(victim); serr != nil {
		t.Fatalf("a file OUTSIDE the notes base was deleted through a symlinked "+
			"workspace directory (delete returned %v)", err)
	}
	if err == nil {
		t.Error("deleting through a symlinked workspace dir reported success")
	}
}

// The paired positive: an ordinary legacy delete still works, so refusing
// everything would not pass.
func TestLegacyDeleteRemovesAnOrdinaryNote(t *testing.T) {
	base := t.TempDir()
	p := seedLegacy(t, base, 1, "track.sql", "select 1")
	if err := OpenLegacyNotes(base).Delete(1, "track.sql"); err != nil {
		t.Fatalf("ordinary delete failed: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the note is still on disk: %v", err)
	}
}

// The personal store's delete shares the same boundary.
func TestPersonalDeleteRefusesASymlinkedWorkspaceDir(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.sql")
	if err := os.WriteFile(victim, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ns.Root(), "ws-1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_ = ns.Delete(1, "victim.sql")
	if _, serr := os.Stat(victim); serr != nil {
		t.Fatal("the personal store deleted a file outside its root")
	}
}

// Criterion 36 — the legacy space is open to every AUTHENTICATED user, not to
// nobody. Deleting is destructive, so it needs an identity even though reading
// does not distinguish between them.
func TestLegacyDeleteRequiresSignIn(t *testing.T) {
	m := unconnected()
	base := t.TempDir()
	p := seedLegacy(t, base, 1, "track.sql", "select 1")
	m.legacy = OpenLegacyNotes(base)
	m.notes = nil // not signed in

	m.explorer.confirmDeleteLegacy("lnote:1:" + encSeg("track.sql"))

	if _, err := os.Stat(p); err != nil {
		t.Errorf("a legacy note was deleted with nobody signed in: %v", err)
	}
}

// Criterion 37 — a note that was unlinked but not durably synced is reported as
// UNCERTAIN, not as a failure, because the file is already gone and a retry
// would act on whatever next holds that name.
func TestRemovedNotDurableIsDistinctFromFailure(t *testing.T) {
	if !errors.Is(ErrRemovedNotDurable, ErrRemovedNotDurable) {
		t.Fatal("sentinel is not comparable")
	}
	// A delete failure must NOT be mistaken for the uncertain case.
	base := t.TempDir()
	err := OpenLegacyNotes(base).Delete(1, "missing.sql")
	if errors.Is(err, ErrRemovedNotDurable) {
		t.Error("an ordinary absent-file delete was reported as a durability partial")
	}
}

// lector r2 P2 — a symlinked workspace directory must not EXPOSE files outside
// the base either. Confinement is not only about deletion: the legacy tree is
// the one place every authenticated user can read, so an escape here publishes
// someone else's files to everybody.
func TestLegacyReadAndListCannotEscapeTheBase(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.sql"), []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "ws-1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	l := OpenLegacyNotes(base)

	if names, _ := l.List(1); len(names) != 0 {
		t.Errorf("List exposed files outside the base: %v", names)
	}
	if body, err := l.Read(1, "secret.sql"); err == nil {
		t.Errorf("Read returned outside contents: %q", body)
	}
}

// lector r2 P1 — the PERSONAL store's write paths are confined too, not just
// its delete. A symlinked ws dir must not let Create write outside the root.
func TestPersonalCreateCannotEscapeTheRoot(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ns.Root(), "ws-1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ns.Create(1, "escaped.sql"); err == nil {
		t.Error("Create succeeded through a symlinked workspace directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.sql")); err == nil {
		t.Error("a note was created OUTSIDE alice's root")
	}
}

// lector r2 P5 — the exported ZERO VALUE must not be usable. `var s NoteStore`
// had no root, so ./ws-1 resolved against the process working directory and an
// ownerless store was expressible after all.
func TestZeroValueNoteStoreIsNotUsable(t *testing.T) {
	var s NoteStore
	if _, err := s.Create(1, "ownerless.sql"); err == nil {
		t.Error("the zero value created a note")
	}
	if _, err := s.List(1); err == nil {
		t.Error("the zero value listed notes")
	}
	if _, _, err := s.Load(1, "x.sql"); err == nil {
		t.Error("the zero value loaded a note")
	}
	if _, err := os.Stat("ws-1"); err == nil {
		os.RemoveAll("ws-1")
		t.Error("the zero value created ws-1 in the working directory")
	}
}
