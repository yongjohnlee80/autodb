package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// notes_flow_test.go holds the note in the query buffer: naming, saving,
// opening, and never losing unsaved work quietly.

// notesHost is the seeded server's host, signed in as root, with its notes
// under base.
func notesHost(t *testing.T) (*tuiapp.Host, *decltest.Screen, string) {
	t.Helper()
	base := t.TempDir()
	session := tuiapp.NewSession(seeded(t), logger.Nop{}, nil)
	t.Cleanup(session.Close)
	h, s := tuiapp.RunHost(t, session, tuiapp.PersonalNotesIn(base), tuiapp.Options{}, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	s.WaitForText(t, "main (1)")
	return h, s, base
}

// newNote names a note through SPC n: the workspace (the first), Tab, the name,
// Enter.
func newNote(t *testing.T, s *decltest.Screen, name string) {
	t.Helper()
	s.Keys(t, key(' '), key('n'))
	s.WaitForText(t, "┌ new note ")
	keys := append([]tuicore.Event{tab()}, decltest.Type(name)...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "created", func(sc string) bool {
		return !strings.Contains(sc, "┌ new note ") && strings.Contains(sc, "created "+name+".sql")
	})
}

// A new note is named, shows in its workspace's notes, is marked unsaved when
// edited, and SPC s writes it.
func TestANoteIsNamedEditedAndSaved(t *testing.T) {
	_, s, base := notesHost(t)
	newNote(t, s, "report")
	s.WaitFor(t, "the open note on the status line", func(sc string) bool { return strings.Contains(lastRow(s), "report.sql") })
	typeInto(t, s, "select 1")
	s.WaitFor(t, "unsaved", func(string) bool { return strings.Contains(lastRow(s), "report.sql [+]") })
	s.Keys(t, key(' '), key('s'))
	s.WaitFor(t, "saved", func(string) bool {
		return strings.Contains(lastRow(s), "saved report.sql") && !strings.Contains(lastRow(s), "[+]")
	})
	files, _ := filepath.Glob(filepath.Join(base, "u-root", "*", "report.sql"))
	if len(files) != 1 {
		t.Fatalf("report.sql on disk: %v", files)
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), "select 1") {
		t.Errorf("report.sql holds %q, want the query", b)
	}
	// The explorer lists it under the workspace's notes.
	s.Keys(t, key(' '), key('e'), enter()) // main opens: connections, notes
	s.WaitForText(t, "notes")
	s.Keys(t, key('j'), key('j'), enter()) // notes
	s.WaitFor(t, "the note listed", func(string) bool { return rowUnder(s, "▾ notes", "report.sql") })
}

// Over unsaved edits, switching user asks first: Stay keeps the edits,
// Discard moves on to the login.
func TestUnsavedWorkIsAskedAboutBeforeSwitchingUser(t *testing.T) {
	_, s, _ := notesHost(t)
	newNote(t, s, "draft")
	typeInto(t, s, "select 2")
	s.WaitFor(t, "unsaved", func(string) bool { return strings.Contains(lastRow(s), "draft.sql [+]") })
	s.Keys(t, key(' '), key('L'))
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('t')) // Stay
	s.WaitFor(t, "stayed", func(sc string) bool {
		return !strings.Contains(sc, "┌ unsaved note ") && !strings.Contains(sc, "┌ sign in ") && strings.Contains(lastRow(s), "draft.sql [+]")
	})
	s.Keys(t, key(' '), key('L'))
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('d')) // Discard
	s.WaitForText(t, "┌ sign in ")
}

// Over unsaved edits, a new note asks first: Stay keeps the edits and names
// nothing; Save writes them and then names the new one; Discard drops them and
// then names it.
func TestUnsavedWorkIsAskedAboutBeforeANewNote(t *testing.T) {
	_, s, base := notesHost(t)
	newNote(t, s, "draft")
	typeInto(t, s, "select 2")
	s.WaitFor(t, "unsaved", func(string) bool { return strings.Contains(lastRow(s), "draft.sql [+]") })
	onDisk := func(name string) string {
		files, _ := filepath.Glob(filepath.Join(base, "u-root", "*", name))
		if len(files) != 1 {
			return ""
		}
		b, _ := os.ReadFile(files[0])
		return string(b)
	}

	s.Keys(t, key(' '), key('n'))
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('t')) // Stay
	s.WaitFor(t, "stayed", func(sc string) bool {
		return !strings.Contains(sc, "┌ unsaved note ") && strings.Contains(lastRow(s), "draft.sql [+]")
	})
	if strings.Contains(s.String(), "┌ new note ") {
		t.Fatalf("Stay went on to name a new note:\n%s", s.String())
	}

	s.Keys(t, key(' '), key('n'))
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('s')) // Save, then the new note
	s.WaitForText(t, "┌ new note ")
	if got := onDisk("draft.sql"); !strings.Contains(got, "select 2") {
		t.Errorf("Save did not write the draft first: draft.sql holds %q", got)
	}
	keys := append([]tuicore.Event{tab()}, decltest.Type("second")...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "created", func(sc string) bool { return strings.Contains(sc, "created second.sql") })

	typeInto(t, s, "select 3")
	s.WaitFor(t, "unsaved", func(string) bool { return strings.Contains(lastRow(s), "second.sql [+]") })
	s.Keys(t, key(' '), key('n'))
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('d')) // Discard, then the new note
	s.WaitForText(t, "┌ new note ")
	keys = append([]tuicore.Event{tab()}, decltest.Type("third")...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "created", func(sc string) bool { return strings.Contains(sc, "created third.sql") })
	if got := onDisk("second.sql"); strings.Contains(got, "select 3") {
		t.Errorf("Discard wrote the edits: second.sql holds %q", got)
	}
}

// A note written by something else since it was opened is not overwritten:
// the save asks, and Keep leaves both as they were.
func TestASaveOverAChangedNoteAsks(t *testing.T) {
	_, s, base := notesHost(t)
	newNote(t, s, "shared")
	files, _ := filepath.Glob(filepath.Join(base, "u-root", "*", "shared.sql"))
	if len(files) != 1 {
		t.Fatalf("shared.sql on disk: %v", files)
	}
	if err := os.WriteFile(files[0], []byte("written elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	typeInto(t, s, "mine")
	s.Keys(t, key(' '), key('s'))
	s.WaitForText(t, "┌ note changed on disk ")
	s.Keys(t, key('k')) // Keep
	s.WaitFor(t, "kept editing", func(sc string) bool { return !strings.Contains(sc, "┌ note changed on disk ") })
	if b, _ := os.ReadFile(files[0]); string(b) != "written elsewhere" {
		t.Errorf("the note on disk was overwritten: %q", b)
	}
}

// Enter on a note in the explorer opens it: its text is the query buffer.
func TestEnterOnANoteOpensIt(t *testing.T) {
	_, s, _ := notesHost(t)
	newNote(t, s, "first")
	typeInto(t, s, "select 'from first'")
	s.Keys(t, key(' '), key('s'))
	s.WaitFor(t, "saved", func(string) bool { return strings.Contains(lastRow(s), "saved first.sql") })
	newNote(t, s, "second") // an empty buffer now
	s.WaitFor(t, "the buffer emptied", func(sc string) bool { return !strings.Contains(sc, "from first") })

	s.Keys(t, key(' '), key('e'), enter()) // main: connections, notes
	s.WaitForText(t, "notes")
	s.Keys(t, key('j'), key('j'), enter())
	s.WaitFor(t, "the notes", func(string) bool { return rowUnder(s, "▾ notes", "first.sql") })
	s.Keys(t, key('j'), enter())
	s.WaitFor(t, "first opened", func(sc string) bool {
		return strings.Contains(sc, "select 'from first'") && strings.Contains(lastRow(s), "first.sql")
	})
}
