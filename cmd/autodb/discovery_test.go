package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

// FINDING THE DAEMON THROUGH THE STORE'S LEASE, end to end through the real
// binary, as an operator and autodb.nvim meet it.
//
// The split this exists for: two processes over one meta store resolving
// different sockets — a launch without $TMPDIR, a config with its own
// [server] socket. Here each process gets its own XDG_RUNTIME_DIR and the
// same XDG_DATA_HOME, which is exactly that split.

type discoveryEnv struct {
	bin  string
	cfg  string
	data string
}

func newDiscoveryEnv(t *testing.T) discoveryEnv {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("[frontdoor]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return discoveryEnv{bin: buildAutodb(t), cfg: cfg, data: t.TempDir()}
}

// runDir is a short XDG_RUNTIME_DIR: its socket must fit sun_path.
func runDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "adbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func (e discoveryEnv) cmd(run string, args ...string) *exec.Cmd {
	c := exec.Command(e.bin, append([]string{"--config", e.cfg}, args...)...)
	c.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+run, "XDG_DATA_HOME="+e.data,
		"XDG_STATE_HOME="+e.data, "XDG_CONFIG_HOME="+filepath.Join(e.data, "config"))
	return c
}

// serve starts a daemon and waits until it answers a hello.
//
// A HELLO, NOT A CONNECT. The socket accepts a connection as soon as it is
// bound, which is before the daemon has taken the lease and recorded where it
// listens; a cell that starts its second process then reads a record with no
// address. That is what failed on the slower CI runner while passing locally.
// The daemon announces before it serves, so an answered hello means the
// record is complete.
func (e discoveryEnv) serve(t *testing.T, run string) *exec.Cmd {
	t.Helper()
	d := e.cmd(run, "--serve")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Signal(syscall.SIGTERM); _, _ = d.Process.Wait() })
	sock := filepath.Join(run, "autodb.sock")
	for i := 0; i < 400; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := rpc.ProbeHello(ctx, "unix", sock)
		cancel()
		if err == nil {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the daemon never answered a hello on %s", sock)
	return nil
}

func (e discoveryEnv) storeID(t *testing.T) string {
	t.Helper()
	id, _, err := meta.StoreID(config.Meta{Engine: "sqlite", Path: filepath.Join(e.data, "autodb", "meta.db")})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// THE BUG: a second --serve over the same store on another socket used to
// say only "refusing to serve". It now names where the holder answers, and
// still exits 69 — it did not serve. Its own socket file is released.
func TestDiscovery_ASecondServeOnAnotherSocketNamesTheHolder(t *testing.T) {
	e := newDiscoveryEnv(t)
	run1, run2 := runDir(t), runDir(t)
	first := e.serve(t, run1)

	out, err := e.cmd(run2, "--serve").CombinedOutput()
	code := -1
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		code = xe.ExitCode()
	}
	body := string(out)
	if code != exitAlreadyServing {
		t.Fatalf("exit %d, want %d:\n%s", code, exitAlreadyServing, body)
	}
	for _, want := range []string{filepath.Join(run1, "autodb.sock"), fmt.Sprintf("pid %d", first.Process.Pid)} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, body)
		}
	}
	if _, err := os.Stat(filepath.Join(run2, "autodb.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused serve left its socket file behind: %v", err)
	}
}

// --print-endpoint SENDS A FRONTEND TO THE HOLDER, with the store it must see.
func TestDiscovery_PrintEndpointAnswersTheHolderAndItsStore(t *testing.T) {
	e := newDiscoveryEnv(t)
	run1, run2 := runDir(t), runDir(t)
	e.serve(t, run1)

	out, err := e.cmd(run2, "--print-endpoint").Output()
	if err != nil {
		t.Fatalf("--print-endpoint: %v\n%s", err, out)
	}
	want := fmt.Sprintf("unix\t%s\nstore\t%s\n", filepath.Join(run1, "autodb.sock"), e.storeID(t))
	if string(out) != want {
		t.Errorf("--print-endpoint =\n%q\nwant\n%q", out, want)
	}
	// The configured endpoint itself, when it answers.
	out, err = e.cmd(run1, "--print-endpoint").Output()
	if err != nil || !strings.HasPrefix(string(out), "unix\t"+filepath.Join(run1, "autodb.sock")+"\n") {
		t.Errorf("configured answer: %q, %v", out, err)
	}
}

// A STORE THAT DOES NOT EXIST YET is "pending": the frontend spawns and asks
// again, and never takes a listener as this store's daemon meanwhile.
func TestDiscovery_PrintEndpointSaysPendingBeforeTheStoreExists(t *testing.T) {
	e := newDiscoveryEnv(t)
	run := runDir(t)
	out, err := e.cmd(run, "--print-endpoint").Output()
	if err != nil {
		t.Fatalf("--print-endpoint: %v\n%s", err, out)
	}
	want := fmt.Sprintf("unix\t%s\nstore\tpending\n", filepath.Join(run, "autodb.sock"))
	if string(out) != want {
		t.Errorf("--print-endpoint = %q, want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(e.data, "autodb", "meta.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--print-endpoint created the store: %v", err)
	}
}

// ANOTHER STORE'S DAEMON AT THE CONFIGURED ADDRESS: --print-endpoint fails
// rather than send a frontend to it, and --serve's bind race says so rather
// than "already running".
func TestDiscovery_AnotherStoresDaemonAtTheConfiguredAddress(t *testing.T) {
	theirs := newDiscoveryEnv(t)
	run := runDir(t)
	theirs.serve(t, run)
	mine := discoveryEnv{bin: theirs.bin, cfg: theirs.cfg, data: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(mine.data, "autodb"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine.data, "autodb", "meta.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mine.cmd(run, "--print-endpoint").CombinedOutput()
	if err == nil {
		t.Fatalf("--print-endpoint succeeded toward another store's daemon:\n%s", out)
	}
	if !strings.Contains(string(out), "different store") {
		t.Errorf("--print-endpoint's refusal does not say why:\n%s", out)
	}

	out, err = mine.cmd(run, "--serve").CombinedOutput()
	var xe *exec.ExitError
	if !errors.As(err, &xe) || xe.ExitCode() != exitAlreadyServing {
		t.Fatalf("--serve: %v, want exit %d:\n%s", err, exitAlreadyServing, out)
	}
	if !strings.Contains(string(out), "not this config's store") {
		t.Errorf("the bind race calls another store's daemon this one:\n%s", out)
	}
}

// A CONFIG THAT MAY NOT SPAWN, OR A STORE WITH NO FILE IDENTITY, prints the
// configured endpoint and no store line — the explicit no-check mode — and
// gets no locator.
func TestDiscovery_NoCheckModeIsExplicit(t *testing.T) {
	ep := config.Endpoint{Network: "unix", Address: "/nonexistent/autodb.sock"}

	pg := config.Config{Meta: config.Meta{Engine: "postgres", DSN: "postgres://h/db"}}
	n, a, line, err := printedEndpoint(context.Background(), pg, ep)
	if err != nil || n != "unix" || a != ep.Address || line != "" {
		t.Errorf("postgres store: %q %q %q %v", n, a, line, err)
	}
	if locatorFor(pg, ep) != nil {
		t.Error("a postgres store got a locator; its session would never dial")
	}

	client := config.Config{Server: config.Server{ClientOnly: true},
		Meta: config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "m.db")}}
	if _, _, line, err := printedEndpoint(context.Background(), client, ep); err != nil || line != "" {
		t.Errorf("client_only: line %q, %v", line, err)
	}
	if locatorFor(client, ep) != nil {
		t.Error("a client_only config got a locator")
	}
}

// AN UNREADABLE STORE IS AN ERROR, never a silent no-check endpoint.
func TestDiscovery_AnUnreadableStoreFailsPrintEndpoint(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads through the permission this cell removes")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "m.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	cfg := config.Config{Meta: config.Meta{Engine: "sqlite", Path: filepath.Join(locked, "m.db")}}
	ep := config.Endpoint{Network: "unix", Address: filepath.Join(dir, "x.sock")}
	if _, _, line, err := printedEndpoint(context.Background(), cfg, ep); err == nil {
		t.Errorf("an unreadable store printed store line %q with no error", line)
	}
}
