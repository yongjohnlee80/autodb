package tui

// Deleting a legacy note must not escape the base — lector's reproduced probe.
//
// A path-based os.Remove addresses the name twice: once when you check it, once
// when the kernel resolves it. Replacing `<base>/ws-N` with a symlink between
// those two moments made the unlink remove a file in an entirely different
// directory.

import (
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
