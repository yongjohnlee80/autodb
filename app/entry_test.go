package app

// THE PUBLIC ENTRY'S OWN CONTRACT, tested through run with private writers,
// separately from the CLI's binary (exitcode_test.go drives that).
//
// Three properties ADR 0100 §1.1 promises a consumer, and none of them can
// be seen by testing package main — they only exist once Main is a library
// entry:
//
//   - the zero Options is the community build: an unstamped call prints the
//     unstamped build's identity, not blanks (conventions/base-products.md,
//     the zero value is the community build, byte-for-byte);
//   - a build's identity is ITS OWN: two callers may run two builds side by
//     side in one process, so identity is a value threaded from Options, never
//     package state that a later call rewrites under a running server;
//   - the caller's context stops the long-running modes: cancelling Main's
//     ctx ends --serve and --web-ui, which a host embedding autodb relies on.
//     The CLI passes Background and behaves exactly as before.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveConfigAt writes the smallest config --serve accepts, on a unix socket
// under dir so no cell can collide with a real port or another cell's socket.
func serveConfigAt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	body := "[server]\nsocket = " + filepath.Join(dir, "autodb.sock") + "\n"
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

// And --web-ui, the other long-running mode: the same cancelled context must
// end it before it can bind its port, or an embedding host could not stop it
// either.
func TestWebUI_ACancelledContextEndsIt(t *testing.T) {
	dir := t.TempDir()
	cfg := serveConfigAt(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	var code int
	var errOut bytes.Buffer
	go func() {
		defer close(done)
		var out bytes.Buffer
		code = run(ctx, []string{"--web-ui", "--config", cfg}, Options{}, &out, &errOut)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("--web-ui did not end on a cancelled context")
	}
	// 0 would mean a server that served and stopped cleanly, which a
	// cancelled-before-it-started context never is.
	if code == 0 && !strings.Contains(errOut.String(), "cancel") {
		t.Errorf("a cancelled --web-ui exited 0; stderr:\n%s", errOut.String())
	}
}
