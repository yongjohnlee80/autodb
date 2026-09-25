package tui_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// app_test.go holds the QML host's foundation: every QML file it ships is
// sound, and the session lifecycle — connect, what sign-in it needs, disconnect
// and reconnect — reaches the screen through the App sources.

func TestEveryHostQMLFileIsSound(t *testing.T) {
	decltest.Check(t, tuiapp.ProgramOptions(tuiapp.Options{})...)
}

// runHost starts the QML host against a real server.
func runHost(t *testing.T, addr string) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	return runHostSized(t, addr, 100, 32)
}

// runHostSized is runHost on a screen of the given size.
func runHostSized(t *testing.T, addr string, w, h int) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)
	notesFor := tuiapp.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	return tuiapp.RunHost(t, session, notesFor, tuiapp.Options{}, w, h)
}

// lastRow is the status line.
func lastRow(s *decltest.Screen) string {
	rows := strings.Split(strings.TrimRight(s.String(), "\n "), "\n")
	return rows[len(rows)-1]
}

// TestTheHostConnectsAndSaysTheServerNeedsItsFirstUser: a fresh server has no
// users; the host connects, names the backend, and sign-in is bootstrap.
func TestTheHostConnectsAndSaysTheServerNeedsItsFirstUser(t *testing.T) {
	addr := startRealServer(t)
	h, s := runHost(t, addr)
	s.WaitFor(t, "connected, needing its first user", func(string) bool {
		row := lastRow(s)
		return strings.Contains(row, "Backend") && strings.Contains(row, addr) && h.Auth() == "bootstrap"
	})
	s.WaitFor(t, "the connected message", func(sc string) bool { return strings.Contains(sc, "connected — autodb") })
}

// TestTheConnectionToggleDisconnectsAndReconnects: SPC x's command, through the
// catalog. A chosen disconnect STAYS one — the dropped generation's watcher
// stands down rather than redialing — and the second toggle connects again.
func TestTheConnectionToggleDisconnectsAndReconnects(t *testing.T) {
	addr := startRealServer(t)
	h, s := runHost(t, addr)
	s.WaitFor(t, "connected", func(string) bool { return h.Auth() == "bootstrap" })
	h.RunCommand("session.connection_toggle")
	s.WaitFor(t, "disconnected", func(sc string) bool {
		return strings.Contains(sc, "SPC x reconnects") && strings.Contains(lastRow(s), "Backend [disconnected]")
	})
	time.Sleep(200 * time.Millisecond) // a watcher that redialed would be connecting by now
	if a := h.Auth(); a != "disconnected" {
		t.Fatalf("a chosen disconnect redialed: sign-in is %q", a)
	}
	h.RunCommand("session.connection_toggle")
	s.WaitFor(t, "connected again", func(string) bool { return h.Auth() == "bootstrap" })
}

// TestAHostWithNoServerSaysTheConnectFailed: nothing listening, no spawner —
// the status line says so, and sign-in is disconnected.
func TestAHostWithNoServerSaysTheConnectFailed(t *testing.T) {
	h, s := runHost(t, "127.0.0.1:1")
	// The session probes for its whole spawn window before it gives up.
	deadline := time.Now().Add(20 * time.Second)
	for !(strings.Contains(s.String(), "connect failed") && h.Auth() == "disconnected") {
		if time.Now().After(deadline) {
			t.Fatalf("no failure reported:\n%s", s.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBuildingAHostStartsNothingUntilItRuns: construction does not dial, spawn
// or move the session's generation — a second host over the same session, or
// one never run, cannot disturb it. The session starts when Run does.
func TestBuildingAHostStartsNothingUntilItRuns(t *testing.T) {
	addr := startRealServer(t)
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)
	tb := tuicore.NewTestBackend(80, 10)
	h, err := tuiapp.New(session, tuiapp.PersonalNotesIn(t.TempDir()), nil,
		tuiapp.Options{App: []tuicore.AppOption{tuicore.WithBackend(tb), tuicore.WithMinFrameInterval(0)}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if g := session.Gen(); g != 0 || session.Connected() {
		t.Fatalf("an unrun host moved the session: gen %d, connected %v", g, session.Connected())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for !session.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("running the host never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheWebHostJoinsTheSharedConnectionAndNeverRedials: the web's session is
// the gateway's, already connected and shared by the user's tabs. The host
// enters at its current generation — no Connect, no new generation.
func TestTheWebHostJoinsTheSharedConnectionAndNeverRedials(t *testing.T) {
	addr := startRealServer(t)
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)
	if _, err := session.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := session.Gen()
	notesFor := tuiapp.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	_, s := tuiapp.RunHost(t, session, notesFor, tuiapp.Options{Frontend: tuiapp.FrontendWeb}, 100, 12)
	s.WaitFor(t, "the shared connection joined", func(string) bool {
		row := lastRow(s)
		return strings.Contains(row, addr) && !strings.Contains(row, "connecting")
	})
	if g := session.Gen(); g != before {
		t.Fatalf("the web host redialed the shared session: gen %d → %d", before, g)
	}
}
