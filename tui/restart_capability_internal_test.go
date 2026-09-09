package tui

// SPC X MUST NOT STOP A DAEMON IT CANNOT START AGAIN.
//
// Reported from the droplet: "I tried to restart the server with SPC+X, it
// failed to reconnect." The gate on that action was `frontend ==
// FrontendTerminal` — which asks WHO IS ASKING, not WHAT THE ACTION NEEDS.
//
// What it needs is a spawner. `restartServer` shuts the daemon down and relies
// on the disconnect watcher to start a replacement, and that replacement comes
// from Session.spawn. On a service install the client config carries
// `client_only = true` (install_frontdoor.sh writes it deliberately, so a
// config handed to a non-operator cannot become what listens), so spawnFor
// returns NIL. The keystroke was still offered and still executed: it stopped
// a systemd-managed daemon that had exited cleanly — which Restart=on-failure
// does not bring back — and then nothing in the process was allowed to start
// one. The front door stayed down.
//
// So the terminal frontend is a necessary condition and never a sufficient one.

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
)

// withSpawner is `unconnected` with a spawner, i.e. the single-user install
// where SPC X is the supported way to pick up a rebuilt binary.
func withSpawner(opts ...Option) *Model {
	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{},
		func() (string, error) { return "", nil })
	return New(sess, nil, nil, opts...)
}

func hasKey(entries []leaderEntry, key rune) bool {
	for _, e := range entries {
		if e.key == key {
			return true
		}
	}
	return false
}

// A TERMINAL WITH NO SPAWNER DOES NOT OFFER THE ACTION.
func TestRestart_TerminalWithoutASpawnerWithdrawsTheAction(t *testing.T) {
	t.Parallel()

	// POSITIVE CONTROL FIRST. Without it, a gate that withdrew the action
	// unconditionally would satisfy every assertion below — and the action is
	// the supported way to pick up a rebuilt binary on a dev install, so
	// removing it everywhere would be a regression wearing a fix's clothes.
	if !hasKey(withSpawner().leaderEntries(), 'X') {
		t.Fatal("a terminal WITH a spawner lost its restart action; that install is " +
			"exactly the one where SPC X works")
	}

	noSpawn := unconnected() // NewSessionOn(..., nil): client_only, a service install
	if hasKey(noSpawn.leaderEntries(), 'X') {
		t.Error("a terminal that cannot spawn a daemon still offers to restart one; " +
			"pressing it stops a service-managed front door that nothing here may " +
			"start again")
	}
}

// AND THE ACTION ITSELF REFUSES, so anything reaching it another way is safe.
//
// The menu is a convenience; this is the guard. Hiding the entry alone would
// leave the shutdown reachable — the same reasoning the web frontend already
// applies to this action.
func TestRestart_TerminalWithoutASpawnerRefusesAndSaysWhat(t *testing.T) {
	t.Parallel()

	m := unconnected()
	m.restartServer()

	if m.statusMsg == "" {
		t.Fatal("restartServer said nothing at all")
	}
	// DESCRIPTIVE, then ACTIONABLE. The operator's own words for what they
	// wanted to be told: autodb is a system service and cannot be restarted
	// from the TUI. So the message says that first, in those terms, before the
	// setting that causes it or the command that fixes it.
	if !strings.Contains(m.statusMsg, "system service") {
		t.Errorf("the refusal does not say autodb is a system service: %q", m.statusMsg)
	}
	if !strings.Contains(m.statusMsg, "cannot restart") {
		t.Errorf("the refusal does not say the TUI cannot restart it: %q", m.statusMsg)
	}
	// The next thing they need is the command that does work.
	if !strings.Contains(m.statusMsg, "systemctl restart") {
		t.Errorf("the refusal does not name how to restart a service-managed daemon: %q",
			m.statusMsg)
	}
	// And the setting, so an operator whose install is not systemd can still
	// tell why this is refused rather than guessing.
	if !strings.Contains(m.statusMsg, "client_only") {
		t.Errorf("the refusal does not name the setting that causes it: %q", m.statusMsg)
	}
	// It must NOT read as the browser refusal: that one is about a different
	// install and would send the operator looking for a web frontend.
	if strings.Contains(m.statusMsg, "browser") {
		t.Errorf("a terminal install got the web frontend's refusal: %q", m.statusMsg)
	}
}

// THE WEB REFUSAL IS STILL ITS OWN ANSWER. Two different installs cannot
// collapse into one message; the web one is about what the process can do, this
// one about what the configuration permits.
func TestRestart_WebRefusalIsStillDistinct(t *testing.T) {
	t.Parallel()

	web := unconnected(WithFrontend(FrontendWeb))
	web.restartServer()
	if !strings.Contains(web.statusMsg, "not available in the browser") {
		t.Errorf("the web refusal changed: %q", web.statusMsg)
	}
}
