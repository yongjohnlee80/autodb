package webserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
)

// freePort returns a port nothing is listening on. Racy in principle, fine in a
// test: the gateway binds it immediately.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startGateway runs a real gateway against a real daemon and returns its base URL.
func startGateway(t *testing.T, daemonAddr string) (base string, gw *Gateway) {
	t.Helper()
	port := freePort(t)
	gw, err := New(Config{
		Network:   "tcp",
		Addr:      daemonAddr,
		Port:      port,
		NotesRoot: t.TempDir(),
		Log:       logger.Nop{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the gateway did not shut down")
		}
	})

	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		req.Header.Set("Origin", base)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, gw
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the gateway never served")
	return "", nil
}

// webLogin posts credentials and returns the minted ticket.
func webLogin(t *testing.T, base, user, pass string) (ticket string, status int) {
	t.Helper()
	body := fmt.Sprintf(`{"subject":%q,"password":%q}`, user, pass)
	req, err := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var out struct{ Ticket string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Ticket, resp.StatusCode
}

// attach opens the WebSocket, presents the ticket, and reads until a frame with
// cells arrives — which is the only evidence that an App is actually RUNNING and
// rendering, as opposed to a session having been created.
func attach(t *testing.T, base, ticket string) (session string, painted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{base}},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	// A full 80x24 frame is ~60 KB of per-cell JSON, over this client's 32 KiB
	// default. A browser has no such limit; raising it here is the test catching up
	// with reality rather than the product being too chatty. The first version of
	// this test read "message too big" as "nothing was painted", which is exactly
	// the wrong conclusion.
	c.SetReadLimit(4 << 20)

	hello := map[string]any{
		"t": "hello", "ticket": ticket,
		"cols": 80, "rows": 24, "cw": 8.0, "ch": 16.0,
	}
	raw, _ := json.Marshal(hello)
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	for {
		_, data, rerr := c.Read(ctx)
		if rerr != nil {
			return session, painted
		}
		var msg struct {
			T       string `json:"t"`
			Session string `json:"session"`
			Reason  string `json:"reason"`
			Updates []struct {
				S string `json:"s"`
			} `json:"u"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("server sent unparseable json: %s", data)
		}
		switch msg.T {
		case "ready":
			session = msg.Session
		case "bye":
			t.Fatalf("the server said bye: %q", msg.Reason)
		case "frame":
			for _, u := range msg.Updates {
				if strings.TrimSpace(u.S) != "" {
					return session, true
				}
			}
		}
	}
}

// The whole path, once: a browser logs in against the DAEMON's identity layer,
// attaches, and gets a rendering autodb TUI.
//
// This is the test that would have caught "it compiles and serves an empty page".
// A ready frame proves a session was created; only PAINTED CELLS prove an App is
// running and the existing TUI component tree reached a browser.
func TestGateway_LoginAttachAndRender(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	base, gw := startGateway(t, daemon)

	// Fresh daemon: the first login bootstraps the admin, so --web-ui is usable on
	// a new install without running a different frontend first.
	ticket, status := webLogin(t, base, "johno", "a long enough passphrase")
	if status != http.StatusOK {
		t.Fatalf("login on a fresh daemon: HTTP %d", status)
	}
	if ticket == "" {
		t.Fatal("no ticket")
	}

	session, painted := attach(t, base, ticket)
	if session == "" {
		t.Error("no ready frame: the attach never produced a session")
	}
	if !painted {
		t.Error("a session was created but nothing was ever painted — the App is not " +
			"running, or the TUI never reached the browser")
	}
	if n := gw.pool.users(); n != 1 {
		t.Errorf("%d pooled users after one login, want 1", n)
	}
}

// The bootstrap path is ONE-SHOT. Once an admin exists, an unknown user cannot
// become a second one by guessing.
func TestGateway_BootstrapIsOneShot(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	base, _ := startGateway(t, daemon)

	if _, status := webLogin(t, base, "first", "a long enough passphrase"); status != http.StatusOK {
		t.Fatalf("the bootstrap login: HTTP %d", status)
	}
	// A different name with a different password must now be refused rather than
	// bootstrapped: NeedsBootstrap is false, so the path is unreachable.
	if _, status := webLogin(t, base, "second", "another passphrase"); status == http.StatusOK {
		t.Error("a second user bootstrapped themselves in after an admin existed")
	}
	// And a wrong password for the REAL user is refused too.
	if _, status := webLogin(t, base, "first", "wrong"); status == http.StatusOK {
		t.Error("a wrong password was accepted")
	}
}

// Two browser sessions for one user share ONE daemon connection, and the user is
// logged out only when the last of them goes.
func TestGateway_TwoTabsOneConnection(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	base, gw := startGateway(t, daemon)

	t1, status := webLogin(t, base, "johno", "a long enough passphrase")
	if status != http.StatusOK {
		t.Fatalf("first login: HTTP %d", status)
	}
	if _, painted := attach(t, base, t1); !painted {
		t.Fatal("the first tab never painted")
	}

	t2, status := webLogin(t, base, "johno", "a long enough passphrase")
	if status != http.StatusOK {
		t.Fatalf("second login: HTTP %d", status)
	}
	if _, painted := attach(t, base, t2); !painted {
		t.Fatal("the second tab never painted")
	}

	if n := gw.pool.users(); n != 1 {
		t.Errorf("%d pooled users for one person's two tabs, want 1 — the daemon is "+
			"paying for the user's tab count", n)
	}
}

// The second tab's connection must be CLOSED, not abandoned.
//
// A login always dials, because a password has to be proven against the daemon
// and cannot be inferred from another tab's session. When that user already has a
// pooled session the fresh connection is surplus — and dropping it on the floor
// would leak a daemon connection per extra tab, with a live token attached,
// which is precisely the accounting the pool exists to keep.
//
// Counting dials is the only way to see this: pool.users() stays 1 either way, so
// the earlier two-tab test cannot detect it. It stayed green when the close was
// deleted.
func TestGateway_SurplusConnectionIsClosedNotAbandoned(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)

	var mu sync.Mutex
	var dialled []*tuiapp.Session
	counting := func(ctx context.Context) (*tuiapp.Session, error) {
		s := tuiapp.NewSessionOn("tcp", daemon, logger.Nop{}, nil)
		if _, err := s.Connect(ctx); err != nil {
			s.Close()
			return nil, err
		}
		mu.Lock()
		dialled = append(dialled, s)
		mu.Unlock()
		return s, nil
	}

	port := freePort(t)
	gw, err := New(Config{
		Network: "tcp", Addr: daemon, Port: port,
		NotesRoot: t.TempDir(), Log: logger.Nop{}, dial: counting,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		req.Header.Set("Origin", base)
		if resp, derr := http.DefaultClient.Do(req); derr == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, status := webLogin(t, base, "johno", "a long enough passphrase"); status != http.StatusOK {
		t.Fatalf("first login: HTTP %d", status)
	}
	if _, status := webLogin(t, base, "johno", "a long enough passphrase"); status != http.StatusOK {
		t.Fatalf("second login: HTTP %d", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dialled) != 2 {
		t.Fatalf("%d dials for two logins, want 2 — a login must prove its password "+
			"on its own connection", len(dialled))
	}
	if n := gw.pool.users(); n != 1 {
		t.Errorf("%d pooled users, want 1", n)
	}
	// Exactly one of the two is pooled and still logged in; the other is the
	// surplus and must have been logged out on its way out.
	loggedIn := 0
	for _, s := range dialled {
		if s.Token() != "" {
			loggedIn++
		}
	}
	if loggedIn != 1 {
		t.Errorf("%d of 2 dialled connections still hold a token, want 1: the surplus "+
			"connection was abandoned with a live token rather than logged out and "+
			"closed", loggedIn)
	}
}

// Two users must not see each other's notes.
//
// tuiapp.NoteStore reads from disk and disk has no identity, so a single shared
// root would hand every web user everyone else's notes. The terminal frontend
// never had this problem: the OS gave it one user per process. This gateway is one
// process for N users, which is where the whole class of problem comes from.
func TestGateway_NoteRootsAreScopedPerUser(t *testing.T) {
	t.Parallel()
	daemon := startRealServer(t)
	notesBase := t.TempDir()

	port := freePort(t)
	gw, err := New(Config{
		Network: "tcp", Addr: daemon, Port: port,
		NotesRoot: notesBase, Log: logger.Nop{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		req.Header.Set("Origin", base)
		if resp, derr := http.DefaultClient.Do(req); derr == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The first login bootstraps the admin; the admin then creates a second user
	// so there are genuinely two identities on the daemon.
	ticket, status := webLogin(t, base, "alice", "a long enough passphrase")
	if status != http.StatusOK {
		t.Fatalf("bootstrap login: HTTP %d", status)
	}
	if _, painted := attach(t, base, ticket); !painted {
		t.Fatal("alice's session never painted")
	}

	adminSess, err := dialer(daemon)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer adminSess.Close()
	actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer acancel()
	if err := adminSess.Bind().Login(actx, "alice", "a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, cerr := adminSess.Bind().CreateUser(actx, "bob", "bob's long passphrase", "reader"); cerr != nil {
		t.Fatalf("creating a second user: %v", cerr)
	}

	bTicket, status := webLogin(t, base, "bob", "bob's long passphrase")
	if status != http.StatusOK {
		t.Fatalf("bob's login: HTTP %d", status)
	}
	if _, painted := attach(t, base, bTicket); !painted {
		t.Fatal("bob's session never painted")
	}

	// Each user's App built its own root under the base, and they are different.
	aRoot, err := noteRootFor(notesBase, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bRoot, err := noteRootFor(notesBase, "bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{aRoot, bRoot} {
		if _, serr := os.Stat(root); serr != nil {
			t.Errorf("note root %q was never created: %v", root, serr)
		}
	}
	if aRoot == bRoot {
		t.Fatal("both users share one note root")
	}
}
