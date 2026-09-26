package tui_test

import (
	"strings"
	"testing"

	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
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
