package tui

// The legacy tree: visible, migratable, deletable — and frozen for writes by the
// SHAPE of the type rather than by a rule someone must remember.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func seedLegacy(t *testing.T, base string, wsID int64, name, body string) string {
	t.Helper()
	dir := filepath.Join(base, "ws-"+strconv.FormatInt(wsID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The ownerless tree is READ, so files written before ADR-0068 do not vanish
// from the UI while sitting on disk — the one outcome worse than showing them.
func TestLegacyNotesAreVisible(t *testing.T) {
	base := t.TempDir()
	seedLegacy(t, base, 1, "track.sql", "select 1")
	seedLegacy(t, base, 2, "users.sql", "select 2")
	l := OpenLegacyNotes(base)

	ws, err := l.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("workspaces = %v, want two", ws)
	}
	names, err := l.List(1)
	if err != nil || len(names) != 1 || names[0] != "track.sql" {
		t.Fatalf("List(1) = %v, %v", names, err)
	}
	body, err := l.Read(1, "track.sql")
	if err != nil || body != "select 1" {
		t.Fatalf("Read = %q, %v", body, err)
	}
}

// A personal store is NOT how legacy files are reached: it is rooted under
// u-<subject>, so the ownerless tree is invisible to it. This is the isolation
// the whole ADR is for, asserted from the other direction.
func TestPersonalStoreCannotSeeTheLegacyTree(t *testing.T) {
	base := t.TempDir()
	seedLegacy(t, base, 1, "track.sql", "select 1")
	ns, err := NewPersonalNotes(base, "root")
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := ns.List(1); len(names) != 0 {
		t.Errorf("the personal store listed legacy notes: %v", names)
	}
	if dirs, _ := ns.ListWorkspaceDirs(); len(dirs) != 0 {
		t.Errorf("the personal store listed legacy workspaces: %v", dirs)
	}
}

// Ignoring names the store could not have written, rather than guessing at them.
func TestLegacyIgnoresNonCanonicalWorkspaceNames(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{"ws-01", "ws-1x", "ws--1", "ws-0", "ws-"} {
		if err := os.MkdirAll(filepath.Join(base, bad), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedLegacy(t, base, 3, "ok.sql", "x")
	ws, err := OpenLegacyNotes(base).Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0] != 3 {
		t.Errorf("workspaces = %v, want exactly [3] — a name this codebase could "+
			"not have produced must not be presented as a workspace", ws)
	}
}

// A drained workspace disappears from the section. The legacy tree only shrinks,
// so once its last note is migrated or deleted the folder left behind has nothing
// to present — the deprecated section must not linger, empty (Johno, 2026-08-30).
func TestLegacyEmptyWorkspaceIsNotListed(t *testing.T) {
	base := t.TempDir()
	// ws-1 still holds a note; ws-2 was drained to its last note by the user.
	seedLegacy(t, base, 1, "keep.sql", "select 1")
	p := seedLegacy(t, base, 2, "gone.sql", "select 2")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	// A canonical folder that never held a `.sql` note is drained too.
	if err := os.MkdirAll(filepath.Join(base, "ws-3"), 0o700); err != nil {
		t.Fatal(err)
	}

	ws, err := OpenLegacyNotes(base).Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0] != 1 {
		t.Errorf("workspaces = %v, want exactly [1] — a folder with no legacy notes "+
			"must not be presented as a workspace", ws)
	}
}

// The section drains as the last note is deleted THROUGH the type: after Delete
// removes the final note, the workspace is gone from the listing even though its
// (now-empty) folder remains on disk.
func TestLegacyWorkspaceDrainsOnLastDelete(t *testing.T) {
	base := t.TempDir()
	seedLegacy(t, base, 7, "only.sql", "select 1")
	l := OpenLegacyNotes(base)

	if ws, _ := l.Workspaces(); len(ws) != 1 || ws[0] != 7 {
		t.Fatalf("before delete: workspaces = %v, want [7]", ws)
	}
	if err := l.Delete(7, "only.sql"); err != nil {
		t.Fatal(err)
	}
	if ws, _ := l.Workspaces(); len(ws) != 0 {
		t.Errorf("after deleting the last note: workspaces = %v, want none", ws)
	}
}
