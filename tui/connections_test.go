package tui_test

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// connections_test.go holds the connections manager: its rows, and add, edit,
// test and delete through its buttons.

// openConnections is SPC c, waiting for the seeded row.
func openConnections(t *testing.T, s *decltest.Screen) {
	t.Helper()
	s.Keys(t, key(' '), key('c'))
	s.WaitFor(t, "the connections", func(sc string) bool {
		return strings.Contains(sc, "┌ connections ") && strings.Contains(sc, "bravo") && strings.Contains(sc, "session")
	})
}

// A row shows its profile and whether the front door carries it; Test says
// how the connection answered.
func TestTheConnectionsManagerListsAndTests(t *testing.T) {
	_, s := signedIn(t)
	openConnections(t, s)
	if sc := s.String(); !strings.Contains(sc, "PROFILE") || !strings.Contains(sc, "PROXY") {
		t.Fatalf("the table lacks its columns:\n%s", sc)
	}
	s.Keys(t, key('t'))
	s.WaitForText(t, "test bravo: ok")
}

// Edit starts on the connection as it is, and a rename is the one change sent.
func TestEditingAConnectionRenamesIt(t *testing.T) {
	_, s := signedIn(t)
	openConnections(t, s)
	s.Keys(t, key('e'))
	s.WaitForText(t, "┌ edit bravo ")
	keys := append([]tuicore.Event{decltest.Ctrl('u')}, decltest.Type("charlie")...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "renamed", func(sc string) bool {
		return strings.Contains(sc, "edit bravo: ok") && strings.Contains(sc, "charlie")
	})
}

// Add asks a name, an engine and a DSN, and the new row is listed.
func TestAddingAConnection(t *testing.T) {
	_, s := signedIn(t)
	openConnections(t, s)
	s.Keys(t, key('a'))
	s.WaitForText(t, "┌ new connection ")
	keys := decltest.Type("delta")
	keys = append(keys, tab(), enter()) // the engine choices, open
	s.Keys(t, keys...)
	s.WaitForText(t, "sqlite")
	for _, n := range engine.All() { // down to sqlite, then choose it
		if n == engine.SQLite {
			break
		}
		s.Keys(t, tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyDown})
	}
	s.Keys(t, enter())
	keys = append([]tuicore.Event{tab()}, decltest.Type(t.TempDir()+"/d.db")...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "created", func(sc string) bool { return strings.Contains(sc, "create delta: ok") && strings.Contains(sc, "delta") })
}

// Delete asks first; Keep leaves the connection, Delete sends the request and
// shows the daemon's answer — bravo has recorded history, which the daemon
// keeps it for.
func TestDeletingAConnectionAsksFirst(t *testing.T) {
	_, s := signedIn(t)
	openConnections(t, s)
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete connection ")
	s.Keys(t, key('k')) // Keep
	s.WaitFor(t, "kept", func(sc string) bool { return !strings.Contains(sc, "┌ delete connection ") && strings.Contains(sc, "bravo") })
	s.Keys(t, key('d'))
	s.WaitForText(t, "┌ delete connection ")
	s.Keys(t, key('d')) // Delete
	s.WaitForText(t, "delete bravo: exec: connection has recorded history")
}
