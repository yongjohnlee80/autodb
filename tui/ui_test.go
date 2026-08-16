package tui_test

// Headless end-to-end drive of the FULL TUI against a REAL server: boot,
// bootstrap the root user through the form, create a connection through
// the manager float, run a query with the vim editor, and read the results
// off the virtual grid. This is the M6 exit gate in test form.

import (
	"context"
	"fmt"
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
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// startRealServer boots a full autodb server on a loopback port.
func startRealServer(t *testing.T) (addr string) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "e2e", rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	for srv.Addr() == "" {
		time.Sleep(time.Millisecond)
	}
	return srv.Addr()
}

type uiHarness struct {
	t    *testing.T
	tb   *tuicore.TestBackend
	app  *tuicore.App
	done chan error
}

func startUI(t *testing.T, addr string) *uiHarness {
	t.Helper()
	notes, err := tuiapp.NewNoteStore(filepath.Join(t.TempDir(), "notes"))
	if err != nil {
		t.Fatal(err)
	}
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)

	ctx, cancel := context.WithCancel(context.Background())
	model := tuiapp.New(session, notes, cancel)
	tb := tuicore.NewTestBackend(110, 32)
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(tb), tuicore.WithMinFrameInterval(0))

	h := &uiHarness{t: t, tb: tb, app: app, done: make(chan error, 1)}
	go func() { h.done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil && err != context.Canceled {
				t.Errorf("app.Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("app never exited")
		}
	})
	return h
}

func (h *uiHarness) screen() string { return h.tb.String() }

// waitFor polls the virtual screen for a substring.
func (h *uiHarness) waitFor(what, sub string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.screen(), sub) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("waiting for %s: %q never appeared.\nscreen:\n%s", what, sub, h.screen())
}

func (h *uiHarness) keys(s string) {
	for _, r := range s {
		ev := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: r, Text: string(r)}
		if err := h.tb.Inject(ev); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *uiHarness) key(code rune) {
	text := ""
	if code >= 0x20 && code < 0xE000 {
		text = string(code)
	}
	if err := h.tb.Inject(tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: code, Text: text}); err != nil {
		h.t.Fatal(err)
	}
}

func TestUIFullFlow(t *testing.T) {
	addr := startRealServer(t)
	h := startUI(t, addr)

	// 1. First run: the bootstrap form appears (master passphrase setup).
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	// 2. Create a connection through the manager float (SPC c → a).
	h.key(' ')
	h.waitFor("leader menu", "connections…")
	h.keys("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	h.keys("demo")
	h.key(tuicore.KeyTab)
	h.keys("sqlite")
	h.key(tuicore.KeyTab)
	h.keys(fmt.Sprintf("file:uie2e%d?mode=memory&cache=shared", time.Now().UnixNano()))
	h.key(tuicore.KeyEnter)
	// The modal scrim covers the status bar; assert the reloaded row
	// inside the float instead.
	h.waitFor("created connection row", "demo")
	h.waitFor("created connection engine", "sqlite")
	h.key(tuicore.KeyEscape) // close the manager

	// 3. Create a workspace and attach the connection (server-side ids 1).
	h.key(' ')
	h.waitFor("leader menu", "workspaces…")
	h.keys("w")
	h.waitFor("workspace manager", "a:add")
	h.keys("a")
	h.waitFor("workspace form", "new workspace")
	h.keys("main")
	h.key(tuicore.KeyEnter)
	h.waitFor("workspace row", "main")
	h.key(tuicore.KeyEscape)

	h.key(' ')
	h.keys("c")
	h.waitFor("connections manager", "a:add")
	h.waitFor("connection row loaded", "demo") // rows land async; 'w' needs a selection
	h.keys("w")                                // attach selected conn to a workspace
	h.waitFor("attach form", "attach demo to workspace")
	h.keys("1")
	h.key(tuicore.KeyEnter)
	time.Sleep(300 * time.Millisecond) // let the attach round-trip settle
	h.key(tuicore.KeyEscape)

	// 4. The explorer shows the workspace; walk to the connection so it
	//    becomes the active one.
	h.waitFor("explorer", "main")
	h.key(0x03) // no-op guard; keep event flow moving
	// Focus the explorer pane and navigate: ws → connections → conn.
	h.key(' ')
	h.waitFor("leader menu", "focus explorer")
	h.keys("e")
	h.keys("l")  // expand ws (pre-assembled: shows connections/notes)
	h.keys("jl") // onto "connections", expand (already loaded)
	h.keys("j")  // onto the demo connection
	h.waitFor("active connection", "demo")

	// 5. Write a query with the vim editor and run it via the leader.
	h.key(' ')
	h.keys("q") // focus query editor
	h.keys("i") // insert mode
	h.keys("CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)")
	h.keys("jk") // escape chord
	h.key(' ')
	h.keys("r")
	h.waitFor("DDL ack", "CREATE: ")

	h.keys("ggVG") // select-all in the editor… then replace via insert
	h.key(tuicore.KeyEscape)
	h.keys("gg")
	h.keys("V")
	h.keys("G")
	h.keys("d") // delete the old buffer content
	h.keys("i")
	h.keys("INSERT INTO songs (title) VALUES ('alpha'), ('beta')")
	h.keys("jk")
	h.key(' ')
	h.keys("r")
	h.waitFor("insert ack", "INSERT: 2 affected")

	h.keys("Vd") // clear the single line
	h.keys("i")
	h.keys("SELECT id, title FROM songs ORDER BY id")
	h.keys("jk")
	h.key(' ')
	h.keys("r")
	h.waitFor("result rows", "alpha")
	h.waitFor("result rows", "beta")
	h.waitFor("result summary", "SELECT: 2 row(s)")

	// 6. The WHERE-less guard surfaces as a structured status message.
	h.keys("Vd")
	h.keys("i")
	h.keys("UPDATE songs SET title = 'x'")
	h.keys("jk")
	h.key(' ')
	h.keys("r")
	h.waitFor("guard refusal", "WHERE clause is blocked")
}
