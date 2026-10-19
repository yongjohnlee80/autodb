package autodocplugin

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A workspace's notes are listed in NOTES as the plugin signs in; a note opens in AutoDoc as the
// server's own file of it, <notes dir>/u-<user>/ws-<id>/<name> (file.open_path); New makes one and
// opens it; Delete asks first.
func TestNotesOpenAsTheStoresFiles(t *testing.T) {
	db := newFakeDB(t)
	db.notesDir = filepath.Join(t.TempDir(), "notes")
	db.notes = []string{"a.sql"}
	p, h := connected(t, db)
	var listed [][]string
	p.notesSent = func(n []string) { listed = append(listed, n) }
	p.chooseNotes(1)
	if len(listed) != 1 || !slices.Equal(listed[0], []string{"a.sql"}) || p.notes.ws != 1 {
		t.Fatalf("the notes listed: %q (workspace %d)", listed, p.notes.ws)
	}
	if got := enabled(p.noteRows()); !slices.Equal(got, []string{"note.new", "note.refresh"}) {
		t.Fatalf("with no note chosen the rows are %q", got)
	}
	p.events().NoteCurrent(0)
	if got := enabled(p.noteRows()); !slices.Equal(got, []string{"note.new", "note.open", "note.delete", "note.refresh"}) {
		t.Fatalf("with a note chosen the rows are %q", got)
	}
	p.events().NoteOpen(0)
	want := filepath.Join(db.notesDir, "u-admin", "ws-1", "a.sql")
	if len(h.calls) != 1 || h.calls[0].id != "file.open_path" || h.calls[0].args[0] != want {
		t.Fatalf("opening a.sql called %q, want file.open_path %s", h.calls, want)
	}

	p.events().Menu("note.new")
	p.events().Named("b")
	if c := db.took("notes.create"); len(c) != 1 || c[0].params[2] != "b.sql" {
		t.Fatalf("notes.create took %+v", c)
	}
	if len(h.calls) != 2 || !strings.HasSuffix(h.calls[1].args[0], "b.sql") {
		t.Fatalf("the new note was not opened: %q", h.calls)
	}

	p.events().NoteCurrent(0)
	p.events().Menu("note.delete")
	if len(db.took("notes.delete")) != 0 {
		t.Fatal("Delete ran before its Yes")
	}
	p.events().Confirmed()
	if d := db.took("notes.delete"); len(d) != 1 || d[0].params[2] != "a.sql" {
		t.Fatalf("notes.delete took %+v", d)
	}
}

// A server that names no notes directory has no files to open: the panel says so.
func TestANoteWithNoNotesDirectoryIsSaid(t *testing.T) {
	db := newFakeDB(t)
	db.notes = []string{"a.sql"}
	p, h := connected(t, db)
	p.chooseNotes(1)
	p.events().NoteOpen(0)
	if len(h.calls) != 0 || !strings.Contains(news(p), "names no notes directory") {
		t.Fatalf("calls %q, the problem %q", h.calls, said(p))
	}
}
