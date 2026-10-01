package tui_test

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"

	"github.com/yongjohnlee80/autodb/core/notes"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// startRig is the TUI started with start, its local transport at local (""
// for an address nothing listens on), counting every attempt to start a
// local daemon.
type startRig struct {
	*remoteRig
	spawns *atomic.Int32
}

func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func newStartRig(t *testing.T, start tuiapp.Start, local string, withProfile bool) *startRig {
	t.Helper()
	srv := remotetest.Start(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.toml")
	if withProfile {
		p, _ := srv.Profile(t)
		p.Name = "staging"
		// Listed first, so a start that ignores which profile it names
		// lands here instead.
		decoy := remoteclient.Profile{ID: "decoy", Name: "elsewhere", Host: "127.0.0.1", Port: 1, User: "alice"}
		if err := remoteclient.SaveProfiles(path, []remoteclient.Profile{decoy, p}); err != nil {
			t.Fatal(err)
		}
	}
	if local == "" {
		local = deadAddr(t)
	}
	spawns := &atomic.Int32{}
	spawn := func() (string, error) {
		spawns.Add(1)
		return "", errors.New("no spawning in tests")
	}
	session := tuiapp.NewSessionOn("tcp", local, logger.Nop{}, spawn)
	t.Cleanup(session.Close)
	notesFor := notes.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	h, s := tuiapp.RunHost(t, session, notesFor,
		tuiapp.Options{RemoteProfiles: path, RemoteKeyDir: filepath.Join(dir, "keys"), Start: start}, 140, 36)
	return &startRig{remoteRig: &remoteRig{h: h, s: s, srv: srv, profiles: path}, spawns: spawns}
}

// settled waits a while for anything the program might still start on its
// own, then reports it stayed disconnected and started no local daemon.
func (r *startRig) stayedDisconnected(t *testing.T) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	if n := r.spawns.Load(); n != 0 {
		t.Fatalf("a local daemon was started %d times", n)
	}
	if a := r.h.Auth(); a != "disconnected" {
		t.Fatalf("auth %q; want disconnected (nothing dialed)", a)
	}
}

// --remote / [tui] start = "remote:<profile>": Connect opens at that profile,
// and connecting reaches the server without the local daemon ever being
// dialed or started.
func TestARemoteStartConnectsWithoutTheLocalDaemon(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Remote: "test"}, "", true)
	r.s.WaitFor(t, "Connect at the profile", func(sc string) bool {
		return strings.Contains(sc, "┌ connect to a remote server ") && strings.Contains(sc, "staging") &&
			!strings.Contains(sc, "elsewhere")
	})
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
	if n := r.spawns.Load(); n != 0 {
		t.Fatalf("a remote start started a local daemon %d times", n)
	}
}

// Cancelling the start's Connect leaves the program connected to nothing,
// saying how to go on; nothing local is dialed or started.
func TestCancellingARemoteStartDialsNothing(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Remote: "test"}, "", true)
	r.s.WaitForText(t, "┌ connect to a remote server ")
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "the hint", func(string) bool {
		return strings.Contains(r.h.SourceText("App.status"), "not connected — Remote › Connect…")
	})
	r.stayedDisconnected(t)
}

// The same after a refused connect at start: the session, switched to the
// remote server for it, goes back to the local transport undialed.
func TestCancellingAFailedRemoteStartDialsNothing(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Remote: "test"}, "", true)
	r.s.WaitForText(t, "┌ connect to a remote server ")
	r.answer(t, "not-the-passphrase", "not-the-passphrase")
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitForText(t, "could not connect")
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "the hint", func(string) bool {
		return strings.Contains(r.h.SourceText("App.status"), "not connected — Remote › Connect…")
	})
	r.stayedDisconnected(t)
}

// [tui] start = "ask": this computer or a remote server. Choosing the server
// opens its Connect, and nothing local is started.
func TestAskingWhereToStartOpensTheChosenServersConnect(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Ask: true}, "", true)
	r.s.WaitFor(t, "the question", func(sc string) bool {
		return strings.Contains(sc, "┌ start on ") && strings.Contains(sc, "This computer") && strings.Contains(sc, "staging")
	})
	down := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyDown}
	r.s.Keys(t, down, down, enter()) // past this computer and elsewhere
	r.s.WaitFor(t, "Connect at staging", func(sc string) bool {
		return strings.Contains(sc, "┌ connect to a remote server ") && strings.Contains(sc, "staging") &&
			!strings.Contains(sc, "elsewhere")
	})
	if n := r.spawns.Load(); n != 0 {
		t.Fatalf("choosing a remote server started a local daemon %d times", n)
	}
}

// Choosing this computer dials the local daemon, which asks for a sign-in.
func TestAskingWhereToStartCanChooseThisComputer(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Ask: true}, seeded(t), true)
	r.s.WaitForText(t, "┌ start on ")
	r.s.Keys(t, enter())
	r.s.WaitForText(t, "┌ sign in ")
}

// Closing the question starts connected to nothing.
func TestDecliningTheStartQuestionDialsNothing(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Ask: true}, "", true)
	r.s.WaitForText(t, "┌ start on ")
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "the hint", func(string) bool {
		return strings.Contains(r.h.SourceText("App.status"), "not connected — Remote › Connect…")
	})
	r.stayedDisconnected(t)
}

// With no remote server there is nothing to ask: it starts on this computer.
func TestAskingWithNoRemoteServerStartsLocally(t *testing.T) {
	r := newStartRig(t, tuiapp.Start{Ask: true}, seeded(t), false)
	r.s.WaitForText(t, "┌ sign in ")
}
