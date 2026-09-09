package tui_test

// SPC X ON A SERVICE INSTALL MUST LEAVE THE DAEMON RUNNING.
//
// This is the cell for the reported defect, and it is the one that had no
// counterpart: TestRestartServerFromTUI drives the same key with a WORKING
// SPAWNER — "the stand-in for `autodb --serve` starting again" — so it only
// ever exercised the install where the action completes. The droplet's install
// is the other one.
//
// There, the client config carries `client_only = true`, so Session.spawn is
// nil by design: install_frontdoor.sh writes it so a config handed to somebody
// who is not the operator cannot become what listens. The keystroke was still
// offered and still executed — it stopped a systemd-managed daemon that then
// exited CLEANLY, which `Restart=on-failure` does not bring back — and nothing
// in the process was permitted to start a replacement. "It failed to
// reconnect" was the symptom; the front door being down was the defect.
//
// The assertion is therefore about the DAEMON, not the message: after the
// keystroke the server must still answer. A run that printed a refusal while
// shutting the daemon down would pass a message-only cell and fail at the only
// thing that matters.

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	logger "github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

func TestRestart_ServiceInstallKeepsTheDaemonRunning(t *testing.T) {
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{
		Engine: "sqlite",
		Path:   filepath.Join(t.TempDir(), "meta.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "service-e2e",
		rpc.WithListener(ln))
	runCtx, cancelSrv := context.WithCancel(context.Background())
	t.Cleanup(cancelSrv)
	served := make(chan error, 1)
	go func() { served <- srv.Run(runCtx) }()

	// NIL SPAWNER: the client_only config, which is what a service install
	// hands the operator.
	notesFor := tuiapp.PersonalNotesIn(filepath.Join(t.TempDir(), "notes"))
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)

	appCtx, cancel := context.WithCancel(context.Background())
	model := tuiapp.New(session, notesFor, cancel)
	tb := tuicore.NewTestBackend(110, 32)
	tr := &traceLog{}
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(tb),
		tuicore.WithMinFrameInterval(0), tuicore.WithTrace(tr.add))
	h := &uiHarness{t: t, tb: tb, app: app, done: make(chan error, 1), trace: tr}
	go func() { h.done <- app.Run(appCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("app never exited")
		}
	})

	h.waitFor("about splash", "autodb")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("service-passphrase")
	h.key(tuicore.KeyTab)
	h.keys("service-passphrase")
	h.key(tuicore.KeyEnter)
	h.waitFor("logged in", "logged in as root")

	// THE MENU MUST NOT OFFER IT, and then THE OPERATOR'S OWN KEYSTROKE must
	// not strand the daemon.
	//
	// SPC X is driven rather than restartServer() called, because that is what
	// was pressed on the droplet. A bare `X` is not the action — it reaches the
	// query editor as ordinary vim input — so the handler-level refusal is
	// covered by the internal cell, which calls restartServer directly and
	// asserts what it says. Here the question is only whether the front door
	// is still up afterwards.
	h.key(' ')
	h.waitFor("leader menu", "SPC — commands")
	if menu := h.screen(); strings.Contains(menu, "restart the server") {
		t.Errorf("a client_only install offers to restart the daemon:\n%s", menu)
	}
	h.keys("X")
	// Whether the menu consumed it or closed is not the property; give the
	// shutdown that WOULD have happened time to land before asking.
	time.Sleep(300 * time.Millisecond)

	// THE DAEMON IS STILL SERVING — the whole point.
	//
	// Asked of the SERVER, not of the screen: a live call has to be answered by
	// a process that is still listening, which no status message can fake.
	if !session.Connected() {
		t.Fatal("the session lost its connection, so the keystroke took the daemon down")
	}
	if _, err := session.Bind().NeedsBootstrap(context.Background()); err != nil {
		t.Fatalf("the daemon stopped answering after SPC X on a client_only install: %v", err)
	}
	select {
	case err := <-served:
		t.Fatalf("the server exited: %v", err)
	case <-time.After(200 * time.Millisecond):
		// Still running, which is correct.
	}
}
