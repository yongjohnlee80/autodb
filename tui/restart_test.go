package tui_test

import (
	"strings"
	"testing"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

func TestRestartNeedsASpawnerAndAsksBeforeShuttingDown(t *testing.T) {
	addr := seeded(t)
	session := tuiapp.NewSession(addr, logger.Nop{}, func() (string, error) { return "", nil })
	t.Cleanup(session.Close)
	h, s := tuiapp.RunHost(t, session, tuiapp.PersonalNotesIn(t.TempDir()), tuiapp.Options{}, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	called := make(chan struct{}, 1)
	h.FakeRestart(called)
	s.Keys(t, key(' '))
	s.WaitForText(t, "X  restart the server")
	s.Keys(t, key('X'))
	s.WaitFor(t, "restart question", func(sc string) bool {
		return strings.Contains(sc, "┌ restart server ") && strings.Contains(sc, "Open transactions")
	})
	s.Keys(t, key('n')) // declining answer does not shut down anything
	s.WaitFor(t, "declined restart", func(sc string) bool { return !strings.Contains(sc, "┌ restart server ") })
	select {
	case <-called:
		t.Fatal("No triggered a shutdown")
	default:
	}
	s.Keys(t, key(' '))
	s.WaitForText(t, "X  restart the server")
	s.Keys(t, key('X'))
	s.WaitForText(t, "┌ restart server ")
	s.Keys(t, key('y'))
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("Yes never reached the guarded shutdown handler")
	}
	s.WaitForText(t, "server stopping — reconnecting")
}
