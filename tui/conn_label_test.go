package tui

// connLabel() is the single answer to "what do we call the active connection",
// and three sites render it. Lector's review of b33c97b showed the title test
// passed even when connLabel() returned "" — it asserted only the ABSENCE of
// "no connection" — so the fallback itself was unguarded, and two of the three
// call sites had no control at all.

import (
	"strings"
	"testing"
)

func TestConnLabel_FallbackAndPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn int64
		nm   string
		want string
	}{
		{"no connection at all", 0, "", ""},
		{"no connection, stale name ignored", 0, "leftover", ""},
		{"name known", 7, "bravo", "bravo"},
		{"name missing falls back to the id", 7, "", "connection 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := unconnected()
			m.activeConn, m.activeConnNm = tc.conn, tc.nm
			if got := m.connLabel(); got != tc.want {
				t.Errorf("connLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Call site 2 of 3: the status bar's centre segment. Before the fix an empty
// name dropped the connection from the status bar entirely.
func TestStatusBarCentre_UsesTheConnectionLabel(t *testing.T) {
	m, tb, sync := mounted(t)
	sync(func() {
		m.activeConn, m.activeConnNm = 9, "" // real connection, name not cached
		m.refreshStatus()
	})
	var screen string
	deadline := deadlineFrom()
	for beforeDeadline(deadline) {
		screen = tb.String()
		if strings.Contains(screen, "connection 9") {
			break
		}
		sleepTick()
	}
	if !strings.Contains(screen, "connection 9") {
		t.Errorf("the status bar does not name the active connection.\nscreen:\n%s", screen)
	}
}
