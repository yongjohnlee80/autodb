package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// unconnected builds a Model with a real but unconnected session, which is all
// leaderEntries needs: it asks the session whether it is connected in order to
// label the connect/disconnect entry.
func unconnected(opts ...Option) *Model {
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	return New(sess, nil, nil, opts...)
}

// The daemon-shutdown action must not exist in a frontend that cannot restart it.
//
// In a terminal `SPC X` is safe: the daemon exits and Session.spawn starts a
// replacement. Under `autodb --web-ui` the spawn is nil by design, so the same
// keystroke strands every web session with no way back — including sessions
// belonging to other people (ADR-0061 §2.7).
//
// Internal rather than in ui_test.go, which is package tui_test: the binding table
// is unexported, and testing the rendered menu text instead would assert on
// presentation for a property that is about capability.
func TestLeaderEntries_WebFrontendWithdrawsTheRestartAction(t *testing.T) {
	t.Parallel()

	has := func(entries []leaderEntry, key rune) bool {
		for _, e := range entries {
			if e.key == key {
				return true
			}
		}
		return false
	}

	// The ZERO value is the terminal, so every existing caller keeps its behaviour
	// without having to name it.
	terminal := unconnected()
	if !has(terminal.leaderEntries(), 'X') {
		t.Error("the terminal frontend lost its restart action: the zero value must " +
			"keep existing behaviour")
	}
	if terminal.frontend != FrontendTerminal {
		t.Error("the zero Frontend is not the terminal")
	}

	web := unconnected(WithFrontend(FrontendWeb))
	if has(web.leaderEntries(), 'X') {
		t.Error("the web frontend still offers to restart the daemon, which nothing " +
			"in that process can do — an admin pressing it strands every session, " +
			"including other users'")
	}

	// And the action itself refuses, so anything that reaches it another way is safe
	// too. The menu is a convenience; this is the guard.
	web.restartServer()
	if !strings.Contains(web.statusMsg, "not available in the browser") {
		t.Errorf("restartServer in web mode said %q, want a refusal", web.statusMsg)
	}
}
