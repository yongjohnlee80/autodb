package app

// THE PUBLIC ENTRY'S OWN CONTRACT, tested through run with private writers,
// separately from the CLI's binary (exitcode_test.go drives that).
//
// Three properties a consumer of the public entry relies on, and none of them
// can be seen by testing package main — they only exist once Main is a
// library entry:
//
//   - the zero Options is the community build: an unstamped call prints the
//     unstamped build's identity, not blanks;
//   - a build's identity is ITS OWN: two callers may run two builds side by
//     side in one process, so identity is a value threaded from Options, never
//     package state that a later call rewrites under a running server;
//   - the caller's context stops the long-running modes: cancelling Main's
//     ctx ends --serve and --web-ui, which a host embedding autodb relies on.
//     The CLI passes Background and behaves exactly as before.

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// serveConfigAt writes the smallest config --serve accepts, on a unix socket
// under dir so no cell can collide with a real port or another cell's socket.
func serveConfigAt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	body := "[server]\nsocket = \"" + filepath.Join(dir, "autodb.sock") + "\"\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The zero Options is the unstamped community build. A consumer that calls
// Main with Options{} must see the same --version a plain `go build` of
// cmd/autodb printed, not a line of blanks: the unstamped defaults are part
// of the entry's contract, not an accident of the ldflags variables' old home.
func TestMain_ZeroOptionsIsTheUnstampedBuild(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--version"}, Options{}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code %d, stderr:\n%s", code, errOut.String())
	}
	if want := "autodb dev (none, built unknown)\n"; out.String() != want {
		t.Errorf("zero Options printed %q; the unstamped build prints %q", out.String(), want)
	}
}

// A stamped build prints its own stamps, and a second call with another
// Options prints THAT one's: identity is read from Options, not from a
// global a previous call left behind.
func TestMain_ABuildsIdentityIsItsOwn(t *testing.T) {
	for _, o := range []Options{
		{Version: "v9.9.9", Commit: "c0ffee", BuildDate: "2026-10-11"},
		{Version: "v0.0.1", Commit: "deadbeef", BuildDate: "2026-01-01"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"--version"}, o, &out, &errOut); code != 0 {
			t.Fatalf("Options %+v: code %d", o, code)
		}
		want := "autodb " + o.Version + " (" + o.Commit + ", built " + o.BuildDate + ")\n"
		if out.String() != want {
			t.Errorf("Options %+v printed %q, want %q", o, out.String(), want)
		}
	}
}

// CONCURRENT builds, DISTINCT identities. Identity was package state in the
// first cut of the move; two callers racing on it could report each other's
// build, and a later call could change the identity a running server already
// reported. Now it is a value threaded from Options, and this cell is the
// pin: run under -race (CI does), a shared global would not survive it.
func TestMain_ConcurrentBuildsKeepTheirOwnIdentity(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	outs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := Options{Version: "build-" + string(rune('a'+i)), Commit: "commit", BuildDate: "2026-10-11"}
			var out, errOut bytes.Buffer
			codes[i] = run(context.Background(), []string{"--version"}, o, &out, &errOut)
			outs[i] = out.String()
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if codes[i] != 0 {
			t.Errorf("build %d: code %d", i, codes[i])
		}
		want := "autodb build-" + string(rune('a'+i)) + " (commit, built 2026-10-11)\n"
		if outs[i] != want {
			t.Errorf("build %d printed %q, want %q", i, outs[i], want)
		}
	}
}

// THE CALLER'S CONTEXT STOPS THE DAEMON. runServe layered its signal context
// on Background, so a host embedding autodb could not stop a server it
// started; now the signal context wraps Main's ctx. A context cancelled
// before the call must end --serve promptly — before the store opens — and
// leave no socket behind.
func TestServe_ACancelledContextEndsIt(t *testing.T) {
	dir := t.TempDir()
	cfg := serveConfigAt(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		// The code is not asserted: reportCode's exact choice for a
		// context-cancelled store open is not the contract; ending is.
		_ = run(ctx, []string{"--serve", "--config", cfg}, Options{Version: "ctx-test"}, &out, &errOut)
		done <- nil
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("--serve did not end on a cancelled context")
	}
	if _, err := os.Stat(filepath.Join(dir, "autodb.sock")); !os.IsNotExist(err) {
		t.Errorf("a cancelled serve left its socket behind (stat: %v)", err)
	}
}

// serveStarted runs --serve on a config of its own until its socket answers a
// dial, and returns how to stop it: cancel ends the daemon through the
// caller's context, and done receives when run has returned.
func serveStarted(t *testing.T, ctx context.Context, dir, version string) (sock string, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cfg := serveConfigAt(t, dir)
	sock = filepath.Join(dir, "autodb.sock")

	ctx, cancel = context.WithCancel(ctx)
	doneCh := make(chan struct{}, 1)
	var serveErr bytes.Buffer
	go func() {
		defer close(doneCh)
		var out bytes.Buffer
		_ = run(ctx, []string{"--serve", "--config", cfg}, Options{Version: version}, &out, &serveErr)
	}()
	done = doneCh

	// THE DAEMON, NOT THE SOCKET FILE: the file exists from the bind, so the
	// cell waits for a dial to be answered — proof it is serving.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return sock, cancel, done
		}
		select {
		case <-done:
			t.Fatalf("--serve exited before it served (dir %s):\n%s", dir, serveErr.String())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the daemon never answered on %s:\n%s", sock, serveErr.String())
		}
	}
}

// A serve that has STARTED must still obey the context. The pre-cancelled
// cell above proves a cancelled context is refused before it serves; this one
// proves the same context ENDS a daemon that is up and answering — the case a
// host embedding autodb actually hits, and the one a background-derived signal
// context used to make impossible.
func TestServe_ACancelledContextEndsARunningDaemon(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data) // the store the fresh daemon opens

	dir := runDir(t) // short: macOS's t.TempDir() puts the socket past sun_path
	sock, cancel, done := serveStarted(t, context.Background(), dir, "ctx-live")
	defer cancel()

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a running --serve did not end when its context was cancelled")
	}
	// The socket the daemon owned is gone: cleanup ran, so a successor can
	// bind, and nothing answers at the address the host told it to leave.
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the ended daemon left its socket behind (stat: %v)", err)
	}
}

// And --web-ui STARTED: a gateway that is already serving a browser ends on
// the caller's context too. The daemon it fronts is the cell's own, started
// the same way, and stopped with it.
func TestWebUI_ACancelledContextEndsARunningGateway(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)

	// A free loopback port for the gateway, picked before anything binds it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(probe.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()

	dir := runDir(t) // short: macOS's t.TempDir() puts the socket past sun_path
	cfg := serveConfigAt(t, dir)
	_, stopServe, serveDone := serveStarted(t, context.Background(), dir, "ctx-live")
	defer stopServe()

	ctx, cancel := context.WithCancel(context.Background())
	gwDone := make(chan struct{}, 1)
	go func() {
		defer close(gwDone)
		var out, errOut bytes.Buffer
		_ = run(ctx, []string{"--web-ui", "--config", cfg, "--port", port}, Options{}, &out, &errOut)
	}()

	// The gateway is UP when it answers the browser's own request.
	url := "http://127.0.0.1:" + port + "/"
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, gerr := http.Get(url)
		if gerr == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case <-gwDone:
			t.Fatal("--web-ui exited before it served")
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			cancel()
			stopServe()
			t.Fatalf("the gateway never answered on %s", url)
		}
	}

	cancel()
	select {
	case <-gwDone:
	case <-time.After(30 * time.Second):
		t.Fatal("a running --web-ui did not end when its context was cancelled")
	}
	stopServe()
	<-serveDone
}
