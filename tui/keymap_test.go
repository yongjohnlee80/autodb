package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/core/notes"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// connectedWithKeymap is connected, the user's key file at file.
func connectedWithKeymap(t *testing.T, file string) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	session := tuiapp.NewSession(startRealServer(t), logger.Nop{}, nil)
	t.Cleanup(session.Close)
	notesFor := notes.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	h, s := tuiapp.RunHost(t, session, notesFor, tuiapp.Options{KeymapFile: file}, 100, 32)
	bootstrapAs(t, h, s, "", "a long enough passphrase")
	return h, s
}

func fkey(code rune) tuicore.KeyEvent { return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: code} }

func writeKeys(t *testing.T, file, xml string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A user's keymap.xml moves AutoDB's keys by id: the Window's leader key opens SPC's card on its new
// key, the card names a moved row by its new key, and the row runs its command on it.
func TestAKeymapMovesAutoDBsKeys(t *testing.T) {
	file := filepath.Join(t.TempDir(), "keymap.xml")
	writeKeys(t, file, `<mapping>
<shortcut id="autodb.window.leader"><key value="F3"/></shortcut>
<shortcut id="autodb.leader.app.about"><key value="F2"/></shortcut>
</mapping>`)
	_, s := connectedWithKeymap(t, file)
	s.Keys(t, fkey(tuicore.KeyF3))
	s.WaitForText(t, "SPC — commands")
	s.WaitForText(t, "F2  about autodb")
	s.Keys(t, fkey(tuicore.KeyF2))
	s.WaitFor(t, "About, on F2", func(sc string) bool {
		return strings.Contains(sc, "About autodb") && !strings.Contains(sc, "SPC — commands")
	})
}

// The key file is followed: created after the start, it moves its keys, and SPC's card says so; a
// broken save is refused, naming the file and line, and the keys stay as they were.
func TestAutoDBFollowsItsKeymapFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "keymap.xml")
	_, s := connectedWithKeymap(t, file)
	writeKeys(t, file, `<mapping><shortcut id="autodb.leader.app.about"><key value="F2"/></shortcut></mapping>`)
	open := func() {
		t.Helper()
		s.Keys(t, decltest.Rune(' '))
		s.WaitForText(t, "SPC — commands")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		open()
		if strings.Contains(s.String(), "F2  about autodb") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the saved file never moved about's key:\n%s", s.String())
		}
		s.Keys(t, esc())
		s.WaitFor(t, "the card closed", func(sc string) bool { return !strings.Contains(sc, "SPC — commands") })
		time.Sleep(100 * time.Millisecond)
	}
	s.Keys(t, esc())
	s.WaitFor(t, "the card closed", func(sc string) bool { return !strings.Contains(sc, "SPC — commands") })
	writeKeys(t, file, "<mapping>\n<shortcut id=\"autodb.leader.app.about\"><key value=\"Ctrl+Nope\"/></shortcut></mapping>")
	s.WaitFor(t, "the refusal, by file and line", func(sc string) bool { return strings.Contains(sc, "keymap.xml, line 2") })
	open()
	if !strings.Contains(s.String(), "F2  about autodb") {
		t.Errorf("after a refused save, the card lost F2:\n%s", s.String())
	}
}
