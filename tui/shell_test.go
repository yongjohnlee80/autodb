package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// shell_test.go holds the screen's shell: the menu bar, the leader menu, the
// hints, help and about cards, quitting, and the themes — each a view of the
// one catalog, reached by the keys the terminal program answered to.

func esc() tuicore.KeyEvent { return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEscape} }

// connected is the host over a real server, signed in as its first user
// through the first-run dialog — on a screen tall enough for the help card's
// whole list.
func connected(t *testing.T) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	h, s := runHostSized(t, startRealServer(t), 100, 32)
	bootstrapAs(t, h, s, "", "a long enough passphrase")
	return h, s
}

func tab() tuicore.KeyEvent { return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyTab} }
func enter() tuicore.KeyEvent {
	return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEnter}
}

// bootstrapAs answers the first-run dialog as a user does — the name (empty
// for root), Tab, the passphrase, Tab, again, Enter — and waits for the
// sign-in.
func bootstrapAs(t *testing.T, h *tuiapp.Host, s *decltest.Screen, user, pass string) {
	t.Helper()
	s.WaitForText(t, "first run — create the root user")
	keys := decltest.Type(user)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(pass)...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(pass)...)
	keys = append(keys, enter())
	s.Keys(t, keys...)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
}

// The menu bar is the catalog's top level: a menu whose every row is planned
// is pruned, as the terminal program's bar was — Edit, until the editor's
// actions are built.
func TestTheMenuBarShowsWhatTheCatalogOffers(t *testing.T) {
	_, s := connected(t)
	top := strings.Split(s.String(), "\n")[0]
	for _, want := range []string{"Home", "Options", "System"} {
		if !strings.Contains(top, want) {
			t.Errorf("the menu bar lacks %q: %q", want, top)
		}
	}
	for _, want := range []string{"File", "Run", "View"} {
		if !strings.Contains(top, want) {
			t.Errorf("the menu bar lacks %q, whose commands are built: %q", want, top)
		}
	}
	for _, pruned := range []string{"Edit"} {
		if strings.Contains(top, pruned) {
			t.Errorf("the menu bar shows %q, whose every command is planned: %q", pruned, top)
		}
	}
}

// SPC opens the leader menu: every built command with a leader key, by key,
// and no planned one. Esc closes it.
func TestSpaceOpensTheLeaderMenu(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "SPC — commands")
	sc := s.String()
	for _, want := range []string{"x  disconnect", "L  login / switch user", "r  run query", "o  profile", "A  about autodb", "?  help", "Q  quit"} {
		if !strings.Contains(sc, want) {
			t.Errorf("the leader menu lacks %q:\n%s", want, sc)
		}
	}
	for _, planned := range []string{"restart the server", "script history"} {
		if strings.Contains(sc, planned) {
			t.Errorf("the leader menu offers %q, which is planned:\n%s", planned, sc)
		}
	}
	s.Keys(t, esc())
	s.WaitFor(t, "the leader menu closed", func(sc string) bool { return !strings.Contains(sc, "SPC — commands") })
}

// A leader key runs its command and closes the menu: A opens About, whose
// backend line follows the connection.
func TestALeaderKeyRunsItsCommand(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "SPC — commands")
	s.Keys(t, decltest.Rune('A'))
	s.WaitFor(t, "About, with the leader closed", func(sc string) bool {
		return strings.Contains(sc, "About autodb") && !strings.Contains(sc, "SPC — commands")
	})
	s.Keys(t, esc())
	s.WaitFor(t, "About closed", func(sc string) bool { return !strings.Contains(sc, "About autodb") })
}

// The leader's x is the connection toggle.
func TestTheLeaderTogglesTheConnection(t *testing.T) {
	h, s := connected(t)
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "x  disconnect")
	s.Keys(t, decltest.Rune('x'))
	s.WaitFor(t, "disconnected", func(string) bool { return h.Auth() == "disconnected" })
}

// x's label follows the connection: once disconnected, the leader offers to
// connect. (Opened on a fresh screen: until the workspace's panes arrive,
// nothing holds focus for a closed card to give back — the query editor will.)
func TestTheLeaderLabelFollowsTheConnection(t *testing.T) {
	h, s := connected(t)
	h.RunCommand("session.connection_toggle")
	s.WaitFor(t, "disconnected", func(string) bool { return h.Auth() == "disconnected" })
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "x  connect")
}

// ? shows the keys that work here, and Esc closes the card.
func TestHintsSayTheKeys(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune('?'))
	s.WaitFor(t, "the hints", func(sc string) bool {
		return strings.Contains(sc, "keys here") && strings.Contains(sc, "the leader menu: every command")
	})
	s.Keys(t, esc())
	s.WaitFor(t, "the hints closed", func(sc string) bool { return !strings.Contains(sc, "keys here") })
}

// Pressure stays in the admin catalog even when this scratch server has no
// meter. Opening it names the missing source instead of drawing a calm table.
func TestPressureOpensAndNamesMissingSource(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "P  front-door pressure")
	s.Keys(t, decltest.Rune('P'))
	s.WaitFor(t, "the unavailable pressure view", func(sc string) bool {
		return strings.Contains(sc, "┌ pressure ") && strings.Contains(sc, "unavailable")
	})
	s.Keys(t, esc())
	s.WaitFor(t, "pressure closed", func(sc string) bool { return !strings.Contains(sc, "┌ pressure ") })
}

// The leader's ? is help: the leader's commands, from the same projection the
// leader runs.
func TestHelpListsTheLeader(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune(' '))
	s.WaitForText(t, "SPC — commands")
	s.Keys(t, decltest.Rune('?'))
	s.WaitFor(t, "help, listing the leader", func(sc string) bool {
		return strings.Contains(sc, "SPC — the leader menu") && strings.Contains(sc, "about autodb")
	})
}

// q asks first: No stays, Yes quits.
func TestQuittingAsksFirst(t *testing.T) {
	_, s := connected(t)
	s.Keys(t, decltest.Rune('q'))
	s.WaitForText(t, "quit autodb?")
	s.Keys(t, decltest.Rune('n'))
	s.WaitFor(t, "stayed", func(sc string) bool { return !strings.Contains(sc, "quit autodb?") })
	select {
	case <-s.Quit():
		t.Fatal("No quit the program")
	default:
	}
	s.Keys(t, decltest.Ctrl('q'))
	s.WaitForText(t, "quit autodb?")
	s.Keys(t, decltest.Rune('y'))
	<-s.Quit()
}

// Options › Theme switches the theme the layout imports, to each theme the
// program ships — only the imported one is linted at build, so a theme that
// does not load is found here — and the menu bar survives each switch.
func TestAThemeCommandSwitchesTheTheme(t *testing.T) {
	h, s := connected(t)
	if got := h.Theme(); got != "retro" {
		t.Fatalf("the screen starts in %q, want retro", got)
	}
	for _, theme := range []string{"mono", "dark", "light", "retro"} {
		h.RunCommand("options.theme." + theme)
		s.WaitFor(t, "the "+theme+" theme", func(sc string) bool {
			return h.Theme() == theme && strings.Contains(sc, "theme: "+theme)
		})
		if top := strings.Split(s.String(), "\n")[0]; !strings.Contains(top, "Options") {
			t.Errorf("%s: the screen lost its menu bar across the switch:\n%s", theme, s.String())
		}
	}
}
