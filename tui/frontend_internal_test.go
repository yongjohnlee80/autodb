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

// The web frontend must not offer, or perform, any action that re-authenticates or
// disconnects its shared session.
//
// The pooled RPC session is shared across a user's tabs (ADR-0061 §2.3). In-App
// login/switch-user would re-key it to another user, and disconnect would drop the
// connection the other tabs are using — the exact hazard lector r3 must-fix 1
// reproduced. So in FrontendWeb these actions are gone from the binding table AND
// the openers refuse, ending the browser App instead.
func TestFrontendWeb_WithdrawsAuthAndConnectionActions(t *testing.T) {
	t.Parallel()

	keys := func(m *Model) map[rune]bool {
		out := map[rune]bool{}
		for _, e := range m.leaderEntries() {
			out[e.key] = true
		}
		return out
	}

	term := keys(unconnected())
	for _, k := range []rune{'L', 'x', 'X'} {
		if !term[k] {
			t.Errorf("the terminal frontend lost SPC %c; the zero value must keep "+
				"existing behaviour", k)
		}
	}

	web := keys(unconnected(WithFrontend(FrontendWeb)))
	for _, k := range []rune{'L', 'x', 'X'} {
		if web[k] {
			t.Errorf("the web frontend still offers SPC %c on a shared per-user "+
				"session — it could re-key or disconnect connections other tabs use", k)
		}
	}
	// It still has to be a usable data client: the query and pane actions remain.
	for _, k := range []rune{'r', 'e', 'q', 't', 'c', 'w', 'u'} {
		if !web[k] {
			t.Errorf("the web frontend lost SPC %c; withdrawing auth actions must not "+
				"strip the data client", k)
		}
	}
}

// openLogin in web mode ENDS the App rather than opening a form, because the App
// cannot re-authenticate a shared session in place.
func TestFrontendWeb_OpenLoginEndsTheApp(t *testing.T) {
	t.Parallel()

	ended := false
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	m := New(sess, nil, func() { ended = true }, WithFrontend(FrontendWeb))

	m.openLogin()
	if !ended {
		t.Error("openLogin in web mode did not end the App — a lost session must " +
			"terminate the browser session, not re-authenticate the shared one")
	}
	if m.modalOpen() {
		t.Error("openLogin in web mode opened a login form on a shared session")
	}

	// openBootstrap likewise: the gateway bootstraps, never the App.
	ended = false
	m.openBootstrap()
	if !ended {
		t.Error("openBootstrap in web mode did not end the App")
	}
}
