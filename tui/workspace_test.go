package tui_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// workspace_test.go holds signing in and the workspace: the explorer, the
// query, and what a run returns — against a server seeded as an operator
// leaves one.

const rootPass = "a long enough passphrase"

// seeded is a server with its root user, a workspace "main", and a SQLite
// connection "bravo" in it holding the table items (two rows).
func seeded(t *testing.T) string {
	t.Helper()
	addr := startRealServer(t)
	sess := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx := context.Background()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	b := sess.Bind()
	cid, err := b.CreateConnection(ctx, "bravo", "sqlite", filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO items (name) VALUES ('ann'), ('bob')`,
	} {
		if _, err := b.Run(ctx, cid, sql); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := b.CreateWorkspace(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AttachConnection(ctx, ws, cid); err != nil {
		t.Fatal(err)
	}
	return addr
}

// loginAs answers the login dialog as a user does: the name — replacing the
// one it remembers, Ctrl+U — Tab, the passphrase, Enter.
func loginAs(t *testing.T, s *decltest.Screen, user, pass string) {
	t.Helper()
	s.WaitForText(t, "┌ sign in ")
	keys := append([]tuicore.Event{decltest.Ctrl('u')}, decltest.Type(user)...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type(pass)...)
	keys = append(keys, enter())
	s.Keys(t, keys...)
}

// signedIn is the host over the seeded server, signed in as root.
func signedIn(t *testing.T) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	h, s := runHostSized(t, seeded(t), 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	return h, s
}

func key(r rune) tuicore.KeyEvent { return decltest.Rune(r) }

func esc2() tuicore.KeyEvent {
	return tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEscape}
}

// typeInto types text into the query editor from Normal mode: i, the text, Esc.
func typeInto(t *testing.T, s *decltest.Screen, text string) {
	t.Helper()
	keys := append([]tuicore.Event{key('i')}, decltest.Type(text)...)
	s.Keys(t, append(keys, esc2())...)
}

// A refused first-run answer opens the dialog again, saying why.
func TestTheFirstRunDialogSaysWhyItRefused(t *testing.T) {
	h, s := runHostSized(t, startRealServer(t), 100, 32)
	s.WaitForText(t, "first run — create the root user")
	keys := append([]tuicore.Event{tab()}, decltest.Type("one passphrase")...)
	keys = append(keys, tab())
	keys = append(keys, decltest.Type("another one")...)
	s.Keys(t, append(keys, enter())...)
	s.WaitFor(t, "the reason", func(sc string) bool {
		return strings.Contains(sc, "first run — create the root user") && strings.Contains(sc, "the passphrases do not match")
	})
	if a := h.Auth(); a != "bootstrap" {
		t.Errorf("a refused first run left sign-in at %q", a)
	}
}

// A wrong passphrase is the server's refusal, on the login dialog's help line
// when it opens again.
func TestAWrongPassphraseIsRefused(t *testing.T) {
	h, s := runHostSized(t, seeded(t), 100, 32)
	loginAs(t, s, "root", "not the passphrase")
	s.WaitFor(t, "refused, and asked again", func(sc string) bool {
		return strings.Contains(sc, "┌ sign in ") && strings.Contains(sc, "bad credentials")
	})
	if a := h.Auth(); a != "login" {
		t.Errorf("a refused login left sign-in at %q", a)
	}
}

// Signed in, the explorer lists the workspaces; Enter opens a folder and makes
// a connection the query's.
func TestEnterOnAConnectionOpensItAndMakesItTheQuerys(t *testing.T) {
	_, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('e'))     // SPC e: the explorer
	s.Keys(t, enter())                // main opens
	s.WaitForText(t, "▸ connections") // the tree row, not the leader card's "connections"
	s.Keys(t, key('j'), enter())      // connections opens
	s.WaitForText(t, "bravo")
	s.Keys(t, key('j'), enter()) // bravo opens, and is the query's
	s.WaitFor(t, "the query targets bravo, opened", func(sc string) bool {
		return strings.Contains(sc, "query → bravo") && rowUnder(s, "bravo sqlite", "main")
	})
}

// Enter on a table scaffolds its SELECT into the query, which does not open;
// SPC r runs it, and the rows are a table whose columns are the query's; SPC
// j shows them as JSON.
func TestATableScaffoldsItsQueryWhichRunsIntoTheResults(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('e'), enter())
	s.WaitForText(t, "▸ connections") // the tree row, not the leader card's "connections"
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "bravo")
	s.Keys(t, key('j'), enter()) // bravo: its schemas
	s.WaitFor(t, "bravo's schema", func(string) bool { return rowUnder(s, "bravo sqlite", "main") })
	s.Keys(t, key('j'), enter()) // the schema: its sections
	s.WaitFor(t, "the sections", func(sc string) bool { return strings.Contains(sc, "▸ tables") && strings.Contains(sc, "▸ views") })
	s.Keys(t, key('j'), enter()) // tables
	s.WaitFor(t, "the tables", func(string) bool { return rowUnder(s, "tables", "items") })
	s.Keys(t, key('j'), enter()) // items: scaffolded
	s.WaitFor(t, "the scaffold, the table not opened", func(sc string) bool {
		return strings.Contains(sc, `SELECT * FROM "main"."items" LIMIT 100`) && strings.Contains(sc, "▸ items")
	})
	s.Keys(t, key(' '), key('r'))
	s.WaitFor(t, "the rows", func(sc string) bool {
		return strings.Contains(sc, "SELECT ok — 2 row(s)") && strings.Contains(sc, "ann") && strings.Contains(sc, "bob") && strings.Contains(sc, "name")
	})
	s.Keys(t, ctrl('j')) // with rows, the results take the keyboard
	s.WaitFor(t, "the results in use", func(string) bool { return h.PaneWithFocus() == "results" })
	s.Keys(t, ctrl('k'), key(' '), key('j'))
	s.WaitFor(t, "the JSON", func(sc string) bool { return strings.Contains(sc, `"name": "ann"`) })
}

// Enter on a result row opens its columns; Enter again shows the chosen value
// without leaving the results pane's selection behind when the cards close.
func TestAResultRowOpensItsFullValue(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('e'), enter())
	s.WaitForText(t, "▸ connections")
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "bravo")
	s.Keys(t, key('j'), enter())
	s.WaitFor(t, "schema", func(string) bool { return rowUnder(s, "bravo sqlite", "main") })
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "▸ tables")
	s.Keys(t, key('j'), enter())
	s.WaitFor(t, "tables", func(string) bool { return rowUnder(s, "tables", "items") })
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, `SELECT * FROM "main"."items" LIMIT 100`)
	s.Keys(t, key(' '), key('r'))
	s.WaitForText(t, "SELECT ok — 2 row(s)")
	s.Keys(t, ctrl('j'), enter())
	s.WaitFor(t, "the first result row's columns", func(sc string) bool {
		return strings.Contains(sc, "┌ row ") && strings.Contains(sc, "name = ann")
	})
	s.Keys(t, key('j'), enter())
	s.WaitFor(t, "the full value", func(sc string) bool {
		return strings.Contains(sc, "┌ name ") && strings.Contains(sc, "ann")
	})
	s.Keys(t, tab(), key(' '))
	s.Keys(t, esc())
	s.WaitForText(t, "┌ row ")
	s.Keys(t, esc())
	s.WaitFor(t, "back to results", func(sc string) bool {
		return h.PaneWithFocus() == "results" && !strings.Contains(sc, "┌ row ") &&
			strings.Contains(sc, "value copied to the editor register")
	})
	s.Keys(t, ctrl('k'), key('p'))
	s.WaitForText(t, "ann")
}

// A run with no connection says so.
func TestARunWithNoConnectionSaysSo(t *testing.T) {
	_, s := signedIn(t)
	s.Keys(t, key(' '), key('r'))
	s.WaitForText(t, "no connection — Enter on one in the explorer")
}

func rows(s *decltest.Screen) []string { return strings.Split(s.String(), "\n") }

// rowUnder reports whether the row after the first one holding above holds
// text — a child shown right under its parent.
func rowUnder(s *decltest.Screen, above, text string) bool {
	rs := rows(s)
	for i, r := range rs {
		if strings.Contains(r, above) {
			return i+1 < len(rs) && strings.Contains(rs[i+1], text)
		}
	}
	return false
}

// rowOf is the first screen row holding text, -1 for none.
func rowOf(s *decltest.Screen, text string) int {
	for i, r := range rows(s) {
		if strings.Contains(r, text) {
			return i
		}
	}
	return -1
}

// SPC C lists the connections; Enter on one makes it the query's, and the
// keyboard goes back to the query.
func TestThePickerChoosesTheQuerysConnection(t *testing.T) {
	h, s := signedIn(t)
	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('C'))
	s.WaitFor(t, "the picker", func(sc string) bool {
		return strings.Contains(sc, "connection for this query") && strings.Contains(sc, "bravo  sqlite")
	})
	s.Keys(t, enter())
	s.WaitFor(t, "bravo chosen", func(sc string) bool {
		return strings.Contains(sc, "query → bravo") && !strings.Contains(sc, "connection for this query")
	})
	s.WaitFor(t, "back on the query", func(string) bool { return h.PaneWithFocus() == "editor" })
}

// A workspace in use with no connections of its own lists every connection,
// as the legacy picker does — and one chosen from that list is in the
// workspace it lives in, not tagged with the empty one.
func TestAConnectionPickedOutsideAnEmptyWorkspaceKeepsItsOwn(t *testing.T) {
	addr := seeded(t)
	admin := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(admin.Close)
	ctx := context.Background()
	if _, err := admin.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := admin.Bind().Login(ctx, "root", rootPass); err != nil {
		t.Fatal(err)
	}
	empty, err := admin.Bind().CreateWorkspace(ctx, "empty")
	if err != nil {
		t.Fatal(err)
	}
	wss, err := admin.Bind().Workspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var main int64
	for _, w := range wss {
		if w.Name == "main" {
			main = w.ID
		}
	}

	h, s := runHostSized(t, addr, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitForText(t, "main (1)")
	h.SetActiveWorkspace(empty)
	s.Keys(t, key(' '), key('C'))
	s.WaitFor(t, "the picker", func(sc string) bool { return strings.Contains(sc, "bravo  sqlite") })
	s.Keys(t, enter())
	s.WaitForText(t, "query → bravo")
	if got := h.ActiveWorkspace(); got != main {
		t.Errorf("bravo was chosen in workspace %d, want main's %d (the empty one is %d)", got, main, empty)
	}
}
