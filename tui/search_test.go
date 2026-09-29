package tui_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestQuerySearchFindsAndWrapsWithoutChangingEditorMode(t *testing.T) {
	h, s := signedIn(t)
	keys := append([]tuicore.Event{key('i')}, decltest.Type("alpha")...)
	keys = append(keys, enter())
	keys = append(keys, decltest.Type("Beta")...)
	keys = append(keys, enter())
	keys = append(keys, decltest.Type("beta")...)
	keys = append(keys, esc())
	s.Keys(t, keys...)
	s.WaitFor(t, "query edit and Escape settled", func(sc string) bool {
		return strings.Contains(sc, "beta") && h.QueryIsNormal()
	})
	h.SetQueryCursor(0, 0)
	if row, _ := h.SearchCursor("query"); row != 0 {
		t.Fatalf("SetLine did not put the query cursor at the search start: %d", row)
	}
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in query ")
	if row, _ := h.SearchCursor("query"); row != 0 {
		t.Fatalf("opening Search moved the query cursor before search ran: %d", row)
	}
	s.Keys(t, decltest.Type("beta")...)
	s.Keys(t, enter())
	s.WaitForText(t, "beta: match 1/2 in the query")
	if row, _ := h.SearchCursor("query"); row != 1 {
		t.Fatalf("first search landed at row %d, want 1", row)
	}
	s.Keys(t, key('n'))
	s.WaitForText(t, "beta: match 2/2 in the query")
	if row, _ := h.SearchCursor("query"); row != 2 {
		t.Fatalf("next search landed at row %d, want 2", row)
	}
	s.Keys(t, key('N'))
	s.WaitForText(t, "beta: match 1/2 in the query")
	if row, _ := h.SearchCursor("query"); row != 1 {
		t.Fatalf("previous search landed at row %d, want 1", row)
	}
	s.Keys(t, key('i'), key('!'), esc())
	s.Keys(t, key('n'))
	s.WaitForText(t, "no current search — / starts one")
}

func TestResultsSearchTargetsTheVisibleTableThenJSON(t *testing.T) {
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
	s.WaitFor(t, "table", func(string) bool { return rowUnder(s, "tables", "items") })
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, `SELECT * FROM "main"."items" LIMIT 100`)
	s.Keys(t, key(' '), key('r'))
	s.WaitForText(t, "SELECT ok — 2 row(s)")
	s.Keys(t, ctrl('j'))
	s.WaitFor(t, "table focused", func(string) bool { return h.PaneWithFocus() == "results" })
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in table ")
	s.Keys(t, decltest.Type("bob")...)
	s.Keys(t, enter())
	s.WaitForText(t, "bob: match 1/1 in the table")
	if row, _ := h.SearchCursor("table"); row != 1 {
		t.Fatalf("table search landed at row %d, want 1", row)
	}
	s.Keys(t, ctrl('k'), key('/'))
	s.WaitForText(t, "┌ find in query ")
	s.Keys(t, esc())
	s.WaitFor(t, "cancelled search closed", func(sc string) bool { return !strings.Contains(sc, "┌ find in query ") })
	s.Keys(t, ctrl('j'), key('n'))
	s.WaitForText(t, "bob: match 1/1 in the table")
	s.Keys(t, ctrl('k'), key(' '), key('j')) // toggle to JSON from query
	s.WaitFor(t, "JSON mode bound", func(sc string) bool {
		return strings.Contains(sc, "[") && strings.Contains(h.SourceText("App.resultsJSON"), `"name": "bob"`)
	})
	s.Keys(t, ctrl('j'))
	s.WaitFor(t, "JSON focused", func(string) bool { return h.PaneWithFocus() == "results" })
	s.Keys(t, key('n'))
	s.WaitForText(t, "no current search — / starts one")
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in json ")
	s.Keys(t, decltest.Ctrl('u'))
	s.Keys(t, decltest.Type("bob")...)
	s.Keys(t, enter())
	s.WaitForText(t, "bob: match 1/1 in the json")
	if row, _ := h.SearchCursor("json"); row < 1 {
		t.Fatalf("JSON search did not move the read-only editor: row %d", row)
	}
	if strings.Contains(s.String(), "┌ find in json ") {
		t.Fatal("accepted search left its modal open")
	}
}

// / in the explorer searches every row the host has loaded, closed folders
// included: a hit under a closed folder opens it and takes the cursor there,
// n goes on from the cursor, and a miss says so.
func TestExplorerSearchRevealsALoadedRowUnderAClosedFolder(t *testing.T) {
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
	s.WaitFor(t, "table", func(string) bool { return rowUnder(s, "tables", "items") })
	// Back to the top and close the workspace: items is loaded but not shown.
	s.Keys(t, key('g'), key('h'))
	s.WaitFor(t, "the workspace closed", func(sc string) bool { return !strings.Contains(sc, "items") })

	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in explorer ")
	s.Keys(t, decltest.Type("ITEMS")...)
	s.Keys(t, enter())
	s.WaitForText(t, "ITEMS: match 1/1 in the explorer")
	s.WaitFor(t, "items revealed and under the cursor", func(sc string) bool {
		at := h.ExplorerCursor()
		return strings.Contains(sc, "items") && len(at) > 0 && strings.HasPrefix(at[len(at)-1], "tbl:") &&
			h.PaneWithFocus() == "explorerTree"
	})
	// n wraps to the only match; the cursor stays on it.
	s.Keys(t, key('n'))
	s.WaitForText(t, "ITEMS: match 1/1 in the explorer")
	// A miss is reported, and the cursor does not move.
	before := h.ExplorerCursor()
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in explorer ")
	s.Keys(t, decltest.Ctrl('u'))
	s.Keys(t, decltest.Type("no-such-row")...)
	s.Keys(t, enter())
	s.WaitForText(t, "no match for no-such-row in the explorer")
	if after := h.ExplorerCursor(); strings.Join(after, "/") != strings.Join(before, "/") {
		t.Fatalf("a miss moved the cursor from %v to %v", before, after)
	}
}

func TestSearchDialogClosesWhenItsWorkspaceIsRetired(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in query ")
	h.RetireWorkspaceForTest()
	s.WaitFor(t, "search closed at workspace retirement", func(sc string) bool {
		return !strings.Contains(sc, "┌ find in query ")
	})
	s.Keys(t, key('n'))
	s.WaitForText(t, "no current search — / starts one")
}

func TestUnicodeQuerySearchLandsAtAGraphemeColumn(t *testing.T) {
	h, s := signedIn(t)
	keys := append([]tuicore.Event{key('i')}, decltest.Type("e\u0301cho café")...)
	s.Keys(t, append(keys, esc())...)
	s.WaitFor(t, "Unicode text and Normal mode", func(sc string) bool {
		return strings.Contains(sc, "café") && h.QueryIsNormal()
	})
	h.SetQueryCursor(0, 0)
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in query ")
	s.Keys(t, decltest.Type("CAFÉ")...)
	s.Keys(t, enter())
	s.WaitForText(t, "CAFÉ: match 1/1 in the query")
	row, col := h.SearchCursor("query")
	if row != 0 || col != 5 {
		t.Fatalf("Unicode match landed at row %d, col %d instead of grapheme column 5", row, col)
	}
}

// seededTwice is one connection with two tables whose names both match
// "items", attached to two workspaces: the same table rows, with the same
// keys, under each.
func seededTwice(t *testing.T) string {
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
		`CREATE TABLE items (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE items_archive (id INTEGER PRIMARY KEY)`,
	} {
		if _, err := b.Run(ctx, cid, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"main", "zeta"} {
		ws, err := b.CreateWorkspace(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.AttachConnection(ctx, ws, cid); err != nil {
			t.Fatal(err)
		}
	}
	return addr
}

// openTables opens the workspace row under the cursor down to its tables:
// the workspace, its connections, the connection, its schema, the tables.
func openTables(t *testing.T, s *decltest.Screen, ws string) {
	t.Helper()
	s.Keys(t, enter())
	s.WaitFor(t, ws+"'s connections", func(sc string) bool { return rowUnder(s, ws, "connections") })
	// Each step waits for the row to appear UNDER its parent: a bare text wait
	// is satisfied too early by a row elsewhere (the schema "main" and the
	// workspace "main").
	for _, step := range []struct{ parent, child string }{
		// The schema is anchored by its indent: "▾ main" alone would find the
		// workspace row "main (1)" first.
		{"connections", "bravo sqlite"}, {"bravo sqlite", "main"}, {"    ▾ main", "tables"}, {"tables", "items"},
	} {
		s.Keys(t, key('j'), enter())
		s.WaitFor(t, step.child+" under "+step.parent, func(string) bool { return rowUnder(s, step.parent, step.child) })
	}
	s.WaitForText(t, "items_archive")
}

// n and N walk every match in tree order and wrap, from the row the cursor is
// on — and "the row" is its whole key path: the same table under a second
// workspace has the same keys, so a cursor kept by its key alone would be
// found under the first workspace again and n would go backwards.
func TestExplorerSearchWalksManyMatchesAndTellsTwinRowsApart(t *testing.T) {
	h, s := runHostSized(t, seededTwice(t), 120, 40)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "mounted", func(sc string) bool { return strings.Contains(sc, "▸ zeta") && h.PaneWithFocus() == "editor" })
	s.Keys(t, key(' '), key('e'))
	s.WaitFor(t, "explorer focused", func(string) bool { return h.PaneWithFocus() == "explorerTree" })
	openTables(t, s, "main")
	s.Keys(t, key('g'), key('h')) // close main; zeta is the next top row
	s.WaitFor(t, "main closed", func(sc string) bool { return !strings.Contains(sc, "items_archive") })
	s.Keys(t, key('j'))
	openTables(t, s, "zeta")
	s.Keys(t, key('g')) // start above every match

	wsOf := func() string {
		at := h.ExplorerCursor()
		if len(at) == 0 {
			return ""
		}
		return at[0]
	}
	s.Keys(t, key('/'))
	s.WaitForText(t, "┌ find in explorer ")
	s.Keys(t, decltest.Type("items")...)
	s.Keys(t, enter())
	var order []string
	for i := 1; i <= 4; i++ {
		want := fmt.Sprintf("items: match %d/4 in the explorer", i)
		s.WaitForText(t, want)
		s.WaitFor(t, "the cursor on match "+strconv.Itoa(i), func(string) bool {
			at := h.ExplorerCursor()
			return len(at) > 0 && strings.HasPrefix(at[len(at)-1], "tbl:")
		})
		at := h.ExplorerCursor()
		order = append(order, wsOf()+" "+at[len(at)-1])
		if i < 4 {
			s.Keys(t, key('n'))
		}
	}
	if order[0][:strings.Index(order[0], " ")] == order[2][:strings.Index(order[2], " ")] {
		t.Fatalf("matches 1 and 3 are under the same workspace %v: the twin under the second workspace was never reached", order)
	}
	if order[0][strings.Index(order[0], " "):] != order[2][strings.Index(order[2], " "):] {
		t.Fatalf("matches 1 and 3 should be the same table under two workspaces: %v", order)
	}
	s.Keys(t, key('n')) // wraps
	s.WaitForText(t, "items: match 1/4 in the explorer")
	s.Keys(t, key('N')) // back to the last
	s.WaitForText(t, "items: match 4/4 in the explorer")
	s.Keys(t, key('N'))
	s.WaitForText(t, "items: match 3/4 in the explorer")
	if wsOf() != order[2][:strings.Index(order[2], " ")] {
		t.Fatalf("N from match 4 landed under %q, want match 3's workspace (%v)", wsOf(), order)
	}
}
