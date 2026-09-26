package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestFileOpenNoteFiltersAndLoadsWithoutLosingUnsavedWork(t *testing.T) {
	_, s, _ := notesHost(t)
	newNote(t, s, "first")
	typeInto(t, s, "select 'saved first'")
	s.Keys(t, key(' '), key('s'))
	s.WaitForText(t, "saved first.sql")
	newNote(t, s, "second")
	typeInto(t, s, "select 'my unsaved second'")
	s.WaitForText(t, "second.sql [+]")
	s.Keys(t, key(' '), key('O'))
	s.WaitForText(t, "┌ open a note ")
	s.WaitFor(t, "both notes listed", func(sc string) bool {
		return strings.Contains(sc, "first.sql") && strings.Contains(sc, "second.sql")
	})
	s.Keys(t, decltest.Type("first")...)
	s.WaitFor(t, "note filter", func(sc string) bool {
		return strings.Contains(sc, "first.sql") && !strings.Contains(sc, "second.sql")
	})
	s.Keys(t, tab(), enter())
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('t')) // Stay: the picker did not discard the open buffer.
	s.WaitFor(t, "stayed with the unsaved note", func(sc string) bool {
		return strings.Contains(sc, "second.sql [+]") && strings.Contains(sc, "my unsaved second")
	})
	s.Keys(t, key(' '), key('O'))
	s.WaitForText(t, "┌ open a note ")
	s.Keys(t, decltest.Type("first")...)
	s.Keys(t, tab(), enter())
	s.WaitForText(t, "┌ unsaved note ")
	s.Keys(t, key('d')) // Discard, then load the selected note.
	s.WaitFor(t, "first note loaded", func(sc string) bool {
		return strings.Contains(sc, "saved first") && strings.Contains(lastRow(s), "first.sql") && !strings.Contains(lastRow(s), "[+]")
	})
}

func TestFileOpenNoteListsDetachedLocalNotesAndRejectsAnAbsentSelection(t *testing.T) {
	_, s, base := notesHost(t)
	store, err := tuiapp.NewPersonalNotes(base, "root")
	if err != nil {
		t.Fatal(err)
	}
	n, err := store.Create(999, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(n, "select 'orphan'"); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, key(' '), key('O'))
	s.WaitFor(t, "detached note", func(sc string) bool {
		return strings.Contains(sc, "detached workspace #999") && strings.Contains(sc, "orphan.sql")
	})
	if err := os.Remove(filepath.Join(base, "u-root", "ws-999", "orphan.sql")); err != nil {
		t.Fatal(err)
	}
	s.Keys(t, tab(), enter())
	s.WaitForText(t, "note no longer exists: orphan.sql")
	if strings.Contains(s.String(), "select 'orphan'") {
		t.Fatal("the missing note replaced the query buffer")
	}
}

func TestLateNoteListingCannotReopenPickerAcrossWorkspaceRetirement(t *testing.T) {
	h, s, _ := notesHost(t)
	started := make(chan struct{}, 1)
	resume := make(chan struct{})
	applied := make(chan struct{}, 1)
	release := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release)
	h.HoldNoteListing(started, resume, applied)
	s.Keys(t, key(' '), key('O'))
	s.WaitForText(t, "┌ open a note ")
	<-started
	h.RetireWorkspaceForTest()
	s.WaitFor(t, "picker retired", func(sc string) bool { return !strings.Contains(sc, "┌ open a note ") })
	release()
	<-applied // the stale worker's answer has reached the UI loop
	if strings.Contains(s.String(), "┌ open a note ") || h.NotePickerRows() != 0 {
		t.Fatal("a stale local listing repopulated the retired picker")
	}
}
