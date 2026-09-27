package tui_test

import (
	"strings"
	"testing"
)

// The history filter narrows the listing on the SERVER: nothing is filtered
// here, so a filter that matches nothing shows nothing, and clearing it
// brings the rows back.
func TestHistoryFilterNarrowsTheListingAndClears(t *testing.T) {
	h, s := signedIn(t)
	s.Keys(t, key(' '), key('H'))
	s.WaitFor(t, "history rows", func(sc string) bool {
		return strings.Contains(sc, "┌ history ") && strings.Contains(sc, "INSERT INTO")
	})
	s.Keys(t, key('f'))
	s.WaitForText(t, "┌ filter history ")
	if err := h.SetTestSource("App.historyFilterStatusIndex", 3); err != nil { // any, ok, ok_pending_commit, error
		t.Fatal(err)
	}
	s.Keys(t, tab(), tab(), tab(), tab(), enter())
	s.WaitFor(t, "an error-only listing", func(sc string) bool {
		return strings.Contains(sc, "status error") && !strings.Contains(sc, "INSERT INTO") && !strings.Contains(sc, "loading…")
	})
	s.Keys(t, key('x'))
	s.WaitFor(t, "the listing again", func(sc string) bool {
		return strings.Contains(sc, "INSERT INTO") && !strings.Contains(sc, "status error")
	})
}

// n and p move through the server's pages by its cursor; the end says so.
func TestHistoryPagesForwardAndBack(t *testing.T) {
	h, s := signedIn(t)
	h.SetHistoryPageSize(1)
	s.Keys(t, key(' '), key('H'))
	s.WaitFor(t, "page 1: the newest row only", func(sc string) bool {
		return strings.Contains(sc, "INSERT INTO") && !strings.Contains(sc, "CREATE") && !strings.Contains(sc, "loading…")
	})
	s.Keys(t, key('n'))
	s.WaitFor(t, "page 2: the older row", func(sc string) bool {
		return strings.Contains(sc, "CREATE") && !strings.Contains(sc, "INSERT INTO") && strings.Contains(sc, "page 2")
	})
	s.Keys(t, key('n'))
	s.WaitForText(t, "this is the last page")
	s.Keys(t, key('p'))
	s.WaitFor(t, "back to page 1", func(sc string) bool {
		return strings.Contains(sc, "INSERT INTO") && !strings.Contains(sc, "CREATE") && !strings.Contains(sc, "page 2")
	})
}
