package tui_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// A STALE PAGE NEVER OVERWRITES A NEWER ONE. Next then prev on a paged list
// runs two loads at once, and the older one's answer can arrive last. Shown,
// it would pair page 2's rows with page 1's cursor, and the next "n" would
// page from the wrong place. So page 2's load is held while "p" asks for page
// 1 again; page 1 lands; then page 2 is released and lands last. The rows
// stay page 1's, and "n" still reaches page 2.
func TestAStaleHistoryPageNeverOverwritesANewerOne(t *testing.T) {
	h, s := signedIn(t)
	h.SetHistoryPageSize(1)
	staleAnswerIsDropped(t, h, s, "history", []rune{' ', 'H'},
		func() string { return fmt.Sprint(h.HistoryScripts()) })
}

func TestAStaleAuditPageNeverOverwritesANewerOne(t *testing.T) {
	h, s := signedIn(t)
	h.SetAuditPageSize(1)
	staleAnswerIsDropped(t, h, s, "audit", []rune{' ', 'a'},
		func() string { return fmt.Sprint(h.AuditIDs()) })
}

func staleAnswerIsDropped(t *testing.T, h *tuiapp.Host, s *decltest.Screen, list string, open []rune, shown func() string) {
	t.Helper()
	answers := make(chan struct{}, 16)
	h.TraceListAnswers(list, answers)
	settled := func(what string, page2 bool) {
		t.Helper()
		s.WaitFor(t, what, func(sc string) bool {
			return strings.Contains(sc, "┌ "+list+" ") && !strings.Contains(sc, "loading…") &&
				strings.Contains(sc, "page 2") == page2
		})
	}
	answer := func(what string) {
		t.Helper()
		select {
		case <-answers:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no answer reached the loop", what)
		}
	}
	for _, r := range open {
		s.Keys(t, key(r))
	}
	settled("page 1", false)
	page1 := shown()
	s.Keys(t, key('n'))
	settled("page 2", true)
	page2 := shown()
	if page1 == page2 {
		t.Fatalf("control: pages 1 and 2 show the same rows (%s), so this cell cannot tell them apart", page1)
	}
	s.Keys(t, key('p'))
	settled("back to page 1", false)
	for len(answers) > 0 {
		<-answers
	}

	started := make(chan chan struct{}, 1)
	h.HoldNextListLoad(list, started)
	s.Keys(t, key('n'))
	var release chan struct{}
	select {
	case release = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("page 2's load never started")
	}
	s.Keys(t, key('p'))
	answer("page 1's answer")
	if got := shown(); got != page1 {
		t.Fatalf("control: page 1's answer shows %s, want %s", got, page1)
	}
	close(release)
	answer("the stale page 2's answer")
	if got := shown(); got != page1 {
		t.Fatalf("the stale page 2 answer replaced page 1: showing %s, want %s", got, page1)
	}
	s.Keys(t, key('n'))
	settled("page 2 again, from page 1's cursor", true)
	if got := shown(); got != page2 {
		t.Fatalf("n after the stale answer shows %s, want page 2's %s", got, page2)
	}
}
