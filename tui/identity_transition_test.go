package tui

// The identity TRANSITION (ADR-0068 §2.2). The store-level guards are tested in
// identity_guard_test.go; these assert the Model actually triggers them, because
// a defence nothing invokes is not a defence.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireIdentityRetiresTheStoreAndBumpsTheEpoch(t *testing.T) {
	m := unconnected()
	base := t.TempDir()
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := ns.Load(1, "draft.sql")
	if err != nil {
		t.Fatal(err)
	}
	m.notes = ns
	before := m.identityEpoch

	m.retireIdentity()

	if m.notes != nil {
		t.Error("the Model still holds a store after retiring the identity")
	}
	if m.identityEpoch == before {
		t.Error("the epoch did not advance, so a delayed result cannot tell it is stale")
	}
	// The retained handle is the danger: a closure that outlived its UI.
	if err := ns.Save(note, "written after the identity ended"); !errors.Is(err, ErrRetired) {
		t.Errorf("a retained handle still wrote through the old store: %v", err)
	}
	if _, serr := readIfExists(filepath.Join(ns.Root(), "ws-1", "draft.sql")); serr == nil {
		t.Error("the body reached disk after the identity was retired")
	}
}

// Retiring twice is safe: several paths can lose an identity at once (a token
// expiring during a switch), and each must be able to call this.
func TestRetireIdentityIsIdempotent(t *testing.T) {
	m := unconnected()
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	m.notes = ns
	m.retireIdentity()
	first := m.identityEpoch
	m.retireIdentity()
	if m.identityEpoch <= first {
		t.Error("a second retire must still advance the epoch")
	}
	if err := ns.alive(); !errors.Is(err, ErrRetired) {
		t.Errorf("store not retired: %v", err)
	}
}

// Dirty work is not silently carried across an identity boundary: retiring
// clears it, so nothing can later save one person's buffer as another's.
func TestRetireIdentityDropsDirtyWork(t *testing.T) {
	m := unconnected()
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	note, _, _ := ns.Load(1, "draft.sql")
	m.notes, m.curNote, m.noteDirty = ns, note, true

	m.retireIdentity()

	if m.curNote != nil || m.noteDirty {
		t.Error("dirty state survived the identity change")
	}
}

// About must not name the configured base before sign-in: that directory is not
// the one this session reads, and naming it suggests the ownerless tree is live.
func TestAboutDoesNotNameTheBaseBeforeSignIn(t *testing.T) {
	m := unconnected()
	base := t.TempDir()
	line := m.notesLine(AboutInfo{NotesDir: base})
	if strings.Contains(line, base) {
		t.Errorf("About names the base %q before sign-in: %q", base, line)
	}
	if !strings.Contains(line, "sign-in") {
		t.Errorf("About does not explain when notes resolve: %q", line)
	}
}

// And after sign-in it names the PERSONAL root, not the base.
func TestAboutNamesThePersonalRootAfterSignIn(t *testing.T) {
	m := unconnected()
	base := t.TempDir()
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	m.notes = ns
	line := m.notesLine(AboutInfo{NotesDir: base})
	if line != ns.Root() {
		t.Errorf("About shows %q, want the personal root %q", line, ns.Root())
	}
	if !strings.Contains(line, "u-alice") {
		t.Errorf("About does not name the identity: %q", line)
	}
}
