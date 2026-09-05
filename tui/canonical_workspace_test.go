package tui

// ADR-0068 criterion 39: a `ws-*` name the store could not have produced must be
// UNADDRESSABLE, not merely unlisted.
//
// wanda's finding, and the fourth time in this ticket that I validated the
// operation a criterion mentioned and left its siblings open: Workspaces()
// refused to list `ws--1`, and Delete(-1, …) removed the file inside it anyway.
// Being invisible in the UI is not the same as being unreachable by the API.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedRawWorkspace writes a note into a workspace directory named EXACTLY as
// given, bypassing the store — that is the point: these are names on disk that
// the store's own formatting could never produce.
func seedRawWorkspace(t *testing.T, base, wsDir, name, body string) string {
	t.Helper()
	dir := filepath.Join(base, wsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The personal store shares the predicate. Confinement stops an escape, but a
// note at `u-alice/ws--1/x.sql` is still a path the store should never name.
func TestPersonalStoreRefusesNonCanonicalWorkspace(t *testing.T) {
	base := t.TempDir()
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	victim := seedRawWorkspace(t, ns.Root(), "ws--1", "note.sql", "still here")

	for name, op := range map[string]func() error{
		"List":   func() error { _, e := ns.List(-1); return e },
		"Load":   func() error { _, _, e := ns.Load(-1, "note.sql"); return e },
		"Create": func() error { _, e := ns.Create(-1, "new.sql"); return e },
		"Delete": func() error { return ns.Delete(-1, "note.sql") },
	} {
		if err := op(); !errors.Is(err, ErrBadWorkspace) {
			t.Errorf("%s(-1) = %v, want ErrBadWorkspace", name, err)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the personal store deleted a file under a rejected name: %v", err)
	}
}

// The paired POSITIVE: canonical ids still work everywhere, so refusing all ids
// would not pass the controls above.
func TestCanonicalWorkspaceStillWorks(t *testing.T) {
	base := t.TempDir()
	ns, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	n, err := ns.Create(1, "real.sql")
	if err != nil {
		t.Fatalf("Create(1): %v", err)
	}
	if err := ns.Save(n, "body"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if names, err := ns.List(1); err != nil || len(names) != 1 {
		t.Fatalf("List(1) = %v, %v", names, err)
	}
	if err := ns.Delete(1, "real.sql"); err != nil {
		t.Fatalf("Delete(1): %v", err)
	}

}
