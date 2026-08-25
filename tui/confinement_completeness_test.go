package tui

// A COMPLETENESS guard for confinement, because enumeration keeps failing.
//
// Three times in this ticket I fixed the operation a finding named and left its
// siblings unconfined, and each time I verified by grepping for `os.` calls.
// Grep is textual: it cannot tell a confined call from an unconfined one, and it
// cannot know which methods exist.
//
// This is behavioural instead. Every operation runs with the process working
// directory set to an empty scratch dir, so ANY filesystem call using a bare
// relative path — `ws-1/note.sql` rather than a path through the confined root —
// lands there and is visible. It does not need to know which methods exist: a
// method added later that forgets confinement fails this without anyone updating
// a list.

import (
	"os"
	"path/filepath"
	"testing"
)

// exerciseEveryOperation runs each public entry point that touches the
// filesystem. A new one that is not called here is still covered by the
// working-directory assertion IF it is reached; the list exists to reach them.
func exerciseEveryOperation(t *testing.T, s *NoteStore, l *LegacyNotes) {
	t.Helper()
	_, _ = s.ListWorkspaceDirs()
	_, _ = s.List(1)
	n, _, _ := s.Load(1, "probe.sql")
	if n != nil {
		_ = s.Save(n, "body")
	}
	if created, err := s.Create(1, "made.sql"); err == nil && created != nil {
		_ = s.Save(created, "more")
	}
	_ = s.Delete(1, "made.sql")

	_, _ = l.Workspaces()
	_, _ = l.List(1)
	_, _ = l.Read(1, "legacy.sql")
	_ = l.Delete(1, "legacy.sql")
}

func TestNoOperationTouchesTheWorkingDirectory(t *testing.T) {
	// A scratch working directory that MUST stay empty.
	cwd := t.TempDir()
	t.Chdir(cwd)

	base := t.TempDir()
	s, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	legacyBase := t.TempDir()
	seedLegacy(t, legacyBase, 1, "legacy.sql", "old")
	l := OpenLegacyNotes(legacyBase)

	exerciseEveryOperation(t, s, l)

	ents, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("an operation wrote to the process working directory: %v\n"+
			"that means a path was used unconfined — a relative path resolved "+
			"against the process CWD instead of through the store's root", names)
	}
}

// The same guard for the ZERO VALUE, which is the case that resolved ./ws-1
// against the working directory before the confined root existed.
func TestZeroValueTouchesNothing(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	var s NoteStore
	var l LegacyNotes
	exerciseEveryOperation(t, &s, &l)

	ents, _ := os.ReadDir(cwd)
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("a zero-value store wrote to the working directory: %v", names)
	}
}

// And the paired POSITIVE: the operations really do work when confined, so a
// store that refused everything would not pass the guards above.
func TestConfinedOperationsActuallyWork(t *testing.T) {
	base := t.TempDir()
	s, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Create(2, "real.sql")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Save(n, "content"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, body, err := s.Load(2, "real.sql"); err != nil || body != "content" {
		t.Fatalf("Load = %q, %v", body, err)
	}
	if names, err := s.List(2); err != nil || len(names) != 1 {
		t.Fatalf("List = %v, %v", names, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "ws-2", "real.sql")); err != nil {
		t.Fatalf("the note is not where the store says it is: %v", err)
	}
	if err := s.Delete(2, "real.sql"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
