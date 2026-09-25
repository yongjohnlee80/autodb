package tui_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// editorpref_test.go holds the editor's keys: chosen from Options › Editor,
// stored on the account, and never one person's for another.

// addUser creates an editor account on the seeded server as root.
func addUser(t *testing.T, addr, name, pass string) {
	t.Helper()
	sess := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx := context.Background()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Bind().CreateUser(ctx, name, pass, "editor"); err != nil {
		t.Fatal(err)
	}
}

// TextEdit chosen from the menu switches the editor at once — letters type
// without an Insert mode — and is the account's: another account that never
// chose gets Vim, and the first gets TextEdit back when it signs in again.
func TestTheEditorsKeysAreTheAccounts(t *testing.T) {
	addr := seeded(t)
	addUser(t, addr, "bob", rootPass)
	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })

	h.RunCommand("options.editor.textedit")
	s.WaitFor(t, "stored", func(string) bool { return strings.Contains(lastRow(s), "editor: textedit") })
	s.Keys(t, key('a'), key('b'), key('c'))
	s.WaitForText(t, "abc") // typed straight in: no Insert mode

	// In TextEdit the editor is modeless — Space types a space — so the
	// switch goes through the command, as the menu would.
	h.RunCommand("session.login")
	loginAs(t, s, "bob", rootPass)
	s.WaitFor(t, "bob's editor", func(string) bool { return h.Auth() == "signed-in" && strings.Contains(lastRow(s), "bob") })
	s.WaitFor(t, "bob never chose: Vim", func(string) bool { return h.Keyset() == "vim" })

	s.Keys(t, key(' '), key('L')) // bob's editor is Vim: Space reaches the leader
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "root's TextEdit back", func(string) bool { return h.Keyset() == "textedit" })
}

// A choice from the menu made while the stored preference is still being read
// wins over it.
func TestAMenuChoiceBeatsASlowRead(t *testing.T) {
	h, s := runHostSized(t, seeded(t), 120, 32)
	reads := make(chan chan map[string]string, 1)
	h.HoldPrefReads(reads)
	loginAs(t, s, "root", rootPass)
	release := <-reads // the read is out
	h.RunCommand("options.editor.textedit")
	s.WaitFor(t, "chosen", func(string) bool { return h.Keyset() == "textedit" })
	before := h.PrefsSettled()
	release <- map[string]string{"editor.keyset": "vim"} // the stored one, late
	s.WaitFor(t, "the read came back", func(string) bool { return h.PrefsSettled() > before })
	if got := h.Keyset(); got != "textedit" {
		t.Errorf("a read that finished after the choice replaced it: %q", got)
	}
}

// One write at a time, and the latest waiting choice is the one written next.
func TestOneWriteAtATimeAndTheLatestWaits(t *testing.T) {
	h, s := signedIn(t)
	started, release := make(chan string, 4), make(chan struct{})
	h.HoldPrefWrites(started, release)
	h.RunCommand("options.editor.textedit")
	if got := <-started; got != "textedit" {
		t.Fatalf("first write %q", got)
	}
	h.RunCommand("options.editor.vim")      // waits
	h.RunCommand("options.editor.textedit") // replaces the waiting one
	s.WaitFor(t, "the choices shown", func(string) bool { return h.Keyset() == "textedit" })
	select {
	case p := <-started:
		t.Fatalf("a second write went out while the first was in flight: %q", p)
	default:
	}
	release <- struct{}{}
	if got := <-started; got != "textedit" {
		t.Errorf("the write after the first was %q, want the latest waiting choice, textedit", got)
	}
	release <- struct{}{}
	// Both writes have come back (the sign-in's read too): anything still to
	// go out would be on started by now.
	s.WaitFor(t, "the second write settled", func(string) bool { return h.PrefsSettled() >= 3 })
	select {
	case p := <-started:
		t.Errorf("a third write went out: %q — the replaced choice was written too", p)
	default:
	}
}
