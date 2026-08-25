package tui

// The cross-identity guard (ADR-0068 §2.2, criterion 8) — lector's r1 probe,
// made permanent.
//
// Before this ADR the probe succeeded: a Note handle minted through alice's
// store could be passed to bob's store's Save and Alice's unsaved body landed in
// u-bob. Nothing about the handle said whose it was, so provenance was the only
// thing distinguishing a legitimate save from a leak, and nothing carried it.

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSaveThroughAnotherIdentitysStoreIsRefused(t *testing.T) {
	base := t.TempDir()
	alice, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := NewPersonalNotes(base, "bob")
	if err != nil {
		t.Fatal(err)
	}

	// Alice mints a handle for a note that does not exist yet — exactly the
	// shape a save-as closure holds.
	aliceNote, _, err := alice.Load(7, "draft.sql")
	if err != nil {
		t.Fatal(err)
	}

	if err := bob.Save(aliceNote, "alice's unsaved body"); !errors.Is(err, ErrForeignNote) {
		t.Fatalf("bob.Save(aliceNote) = %v, want ErrForeignNote — Alice's body must "+
			"not be writable through Bob's store", err)
	}
	if _, serr := readIfExists(filepath.Join(bob.Root(), "ws-7", "draft.sql")); serr == nil {
		t.Error("Alice's body reached u-bob on disk")
	}
}

// A RETIRED store refuses too: the handle is its own, but the identity is gone.
// This is the retained-closure case — dismissing the UI that owns a callback
// does not stop the callback.
func TestSaveThroughARetiredStoreIsRefused(t *testing.T) {
	base := t.TempDir()
	alice, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := alice.Load(7, "draft.sql")
	if err != nil {
		t.Fatal(err)
	}
	alice.Retire()

	if err := alice.Save(note, "written after logout"); !errors.Is(err, ErrRetired) {
		t.Fatalf("Save through a retired store = %v, want ErrRetired", err)
	}
	for _, op := range []func() error{
		func() error { _, e := alice.List(7); return e },
		func() error { _, _, e := alice.Load(7, "draft.sql"); return e },
		func() error { _, e := alice.Create(7, "new.sql"); return e },
		func() error { return alice.Delete(7, "draft.sql") },
	} {
		if err := op(); !errors.Is(err, ErrRetired) {
			t.Errorf("a retired store still served an operation: %v", err)
		}
	}
}

// The paired POSITIVE: an ordinary save through its own live store works.
// Without it, refusing everything would pass both controls above.
func TestSaveThroughItsOwnLiveStoreWorks(t *testing.T) {
	base := t.TempDir()
	alice, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := alice.Load(7, "draft.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Save(note, "alice's own body"); err != nil {
		t.Fatalf("alice cannot save her own note: %v", err)
	}
	got, err := readIfExists(filepath.Join(alice.Root(), "ws-7", "draft.sql"))
	if err != nil {
		t.Fatalf("alice's note is not on disk: %v", err)
	}
	if got != "alice's own body" {
		t.Errorf("body = %q", got)
	}
}
