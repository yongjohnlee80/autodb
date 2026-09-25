package tui_test

import (
	"strings"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// A run issued under one identity and answered after another signed in never
// fills the new identity's results; the new one may run at once.
func TestARunAnsweredAfterTheIdentityChangedIsDropped(t *testing.T) {
	h, s := signedIn(t)
	started := make(chan chan *tuiapp.ExecResult, 2)
	h.HoldRuns(started)

	s.WaitForText(t, "main")
	s.Keys(t, key(' '), key('e'), enter())
	s.WaitForText(t, "▸ connections") // the tree row, not the leader card's "connections"
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "bravo")
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "query → bravo")
	s.Keys(t, key(' '), key('q'))
	typeInto(t, s, "select 'mine'")
	s.Keys(t, key(' '), key('r'))
	release := <-started // the run is in flight, as root's first sign-in

	// Signed in again: the identity changes while the run is pending.
	s.Keys(t, key(' '), key('L'))
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "the identity changed", func(sc string) bool { return strings.Contains(sc, "no connection") })

	release <- &tuiapp.ExecResult{Verb: "select", Columns: []string{"who"}, Rows: [][]any{{"the old identity's row"}}}
	s.WaitFor(t, "the late answer came back", func(string) bool { return h.RunsAnswered() == 1 })
	if sc := s.String(); strings.Contains(sc, "the old identity's row") || !strings.Contains(sc, "no results") {
		t.Fatalf("a run answered after the identity changed filled the pane:\n%s", sc)
	}

	// A new run is admitted at once — the old one's guard did not survive.
	s.Keys(t, key(' '), key('e'), enter())
	s.WaitForText(t, "▸ connections") // the tree row, not the leader card's "connections"
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "bravo")
	s.Keys(t, key('j'), enter())
	s.WaitForText(t, "query → bravo")
	s.Keys(t, key(' '), key('q'))
	typeInto(t, s, "select 'theirs'")
	s.Keys(t, key(' '), key('r'))
	(<-started) <- &tuiapp.ExecResult{Verb: "select", Columns: []string{"who"}, Rows: [][]any{{"the new identity's row"}}}
	s.WaitForText(t, "the new identity's row")
}
