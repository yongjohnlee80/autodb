package tui_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// host_test.go holds the QML host's foundation: every QML file it ships is
// sound, and the session lifecycle — connect, what sign-in it needs, disconnect
// and reconnect — reaches the screen through the App sources.

func TestEveryHostQMLFileIsSound(t *testing.T) {
	decltest.Check(t, tuiapp.HostProgramOptions(tuiapp.HostOptions{})...)
}

// runHost starts the QML host against a real server.
func runHost(t *testing.T, addr string) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)
	notesFor := tuiapp.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	return tuiapp.RunHost(t, session, notesFor, tuiapp.HostOptions{}, 100, 12)
}

// lastRow is the status line.
func lastRow(s *decltest.Screen) string {
	rows := strings.Split(strings.TrimRight(s.String(), "\n "), "\n")
	return rows[len(rows)-1]
}

// TestTheHostConnectsAndSaysTheServerNeedsItsFirstUser: a fresh server has no
// users; the host connects, names the backend, and says sign-in is bootstrap.
func TestTheHostConnectsAndSaysTheServerNeedsItsFirstUser(t *testing.T) {
	addr := startRealServer(t)
	_, s := runHost(t, addr)
	s.WaitFor(t, "connected, needing its first user", func(string) bool {
		row := lastRow(s)
		return strings.Contains(row, "Backend") && strings.Contains(row, addr) && strings.Contains(row, "bootstrap")
	})
	s.WaitFor(t, "the connected message", func(sc string) bool { return strings.Contains(sc, "connected — autodb") })
}

// TestTheConnectionToggleDisconnectsAndReconnects: SPC x's command, through the
// catalog. A chosen disconnect STAYS one — the dropped generation's watcher
// stands down rather than redialing — and the second toggle connects again.
func TestTheConnectionToggleDisconnectsAndReconnects(t *testing.T) {
	addr := startRealServer(t)
	h, s := runHost(t, addr)
	s.WaitFor(t, "connected", func(string) bool { return strings.Contains(lastRow(s), "bootstrap") })
	h.RunCommand("session.connection_toggle")
	s.WaitFor(t, "disconnected", func(sc string) bool {
		return strings.Contains(sc, "SPC x reconnects") && strings.Contains(lastRow(s), "Backend [disconnected]")
	})
	time.Sleep(200 * time.Millisecond) // a watcher that redialed would be connecting by now
	if row := lastRow(s); !strings.Contains(row, "disconnected") {
		t.Fatalf("a chosen disconnect redialed: %q", row)
	}
	h.RunCommand("session.connection_toggle")
	s.WaitFor(t, "connected again", func(string) bool { return strings.Contains(lastRow(s), "bootstrap") })
}

// TestAHostWithNoServerSaysTheConnectFailed: nothing listening, no spawner —
// the status line says so, and sign-in is disconnected.
func TestAHostWithNoServerSaysTheConnectFailed(t *testing.T) {
	_, s := runHost(t, "127.0.0.1:1")
	// The session probes for its whole spawn window before it gives up.
	deadline := time.Now().Add(20 * time.Second)
	for !(strings.Contains(s.String(), "connect failed") && strings.Contains(lastRow(s), "disconnected")) {
		if time.Now().After(deadline) {
			t.Fatalf("no failure reported:\n%s", s.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
