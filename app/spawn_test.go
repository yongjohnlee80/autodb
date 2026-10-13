package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// waitExited waits for a spawned child's exit, bounded so a child that never
// exits fails the cell instead of hanging it.
func waitExited(t *testing.T, sp tuiapp.Spawned) {
	t.Helper()
	select {
	case <-sp.Exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the spawned child never reported its exit")
	}
}

// THE REPORT IS THE CHILD'S OWN: how it exited, and what it wrote, and none of
// what an earlier server left in the shared log.
func TestStartLogged_ReportsTheExitAndOnlyThisChildsOutput(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "serve.log")
	if err := os.WriteFile(logPath, []byte("an earlier server said this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c",
		`echo starting; echo; echo "refusing to serve: held by pid 7" >&2; exit 69`)
	sp, err := startLogged(cmd, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if sp.LogPath != logPath {
		t.Errorf("LogPath %q, want %q", sp.LogPath, logPath)
	}
	waitExited(t, sp)
	want := "exit status 69: starting | refusing to serve: held by pid 7"
	if got := sp.Report(); got != want {
		t.Errorf("Report() = %q, want %q", got, want)
	}
	// Appended, never truncated: the earlier server's line is still in the log.
	if b, _ := os.ReadFile(logPath); !strings.HasPrefix(string(b), "an earlier server said this\n") {
		t.Errorf("the log lost what was in it before:\n%s", b)
	}
}

// A child that exits cleanly and says nothing is still reported as an exit.
func TestStartLogged_ASilentExitSaysHowItExited(t *testing.T) {
	sp, err := startLogged(exec.Command("/bin/sh", "-c", "exit 0"), filepath.Join(t.TempDir(), "serve.log"))
	if err != nil {
		t.Fatal(err)
	}
	waitExited(t, sp)
	if got := sp.Report(); got != "exit status 0" {
		t.Errorf("Report() = %q, want %q", got, "exit status 0")
	}
}

// A log that cannot be opened, or a child that cannot start, is an error from
// the spawn rather than a session left dialing.
func TestStartLogged_FailuresToStartAreErrors(t *testing.T) {
	if _, err := startLogged(exec.Command("/bin/true"), filepath.Join(t.TempDir(), "missing", "serve.log")); err == nil {
		t.Error("an unopenable log was accepted")
	}
	if _, err := startLogged(exec.Command(filepath.Join(t.TempDir(), "no-such-binary")), filepath.Join(t.TempDir(), "serve.log")); err == nil {
		t.Error("a binary that does not exist was started")
	}
}

func TestLogTail_Bounds(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The last reportTailLines non-empty lines, trimmed.
	var many strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&many, "  line %d  \n\n", i)
	}
	if got, want := logTail(write("many", many.String()), 0), "line 5 | line 6 | line 7 | line 8 | line 9 | line 10"; got != want {
		t.Errorf("many lines: %q, want %q", got, want)
	}

	// Past the byte bound, the line the bound cut is dropped, not quoted half.
	long := strings.Repeat("x", reportTailBytes) + "\nlast\n"
	if got := logTail(write("long", long), 0); got != "last" {
		t.Errorf("clipped: %q, want %q", got, "last")
	}

	// From an offset: only what came after it.
	p := write("offset", "before\nafter\n")
	if got := logTail(p, int64(len("before\n"))); got != "after" {
		t.Errorf("from an offset: %q, want %q", got, "after")
	}
	// A log shorter than the offset was replaced under us: all of it is new.
	if got := logTail(p, 1<<20); got != "before | after" {
		t.Errorf("replaced log: %q, want %q", got, "before | after")
	}
	// An unreadable log adds nothing.
	if got := logTail(filepath.Join(dir, "absent"), 0); got != "" {
		t.Errorf("absent log: %q, want empty", got)
	}
}

func TestServeLogPath(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	p, err := serveLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "autodb", "serve.log"); p != want {
		t.Errorf("serveLogPath() = %q, want %q", p, want)
	}
	fi, err := os.Stat(filepath.Dir(p))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log directory: %v, %v; want 0700", fi, err)
	}

	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	if p, err := serveLogPath(); err != nil || p != filepath.Join(home, ".local", "state", "autodb", "serve.log") {
		t.Errorf("without XDG_STATE_HOME: %q, %v", p, err)
	}
}

// spawnFor's spawner starts THIS executable with --serve and --config, its
// output in serve.log. Under `go test` this executable is the test binary,
// which refuses --serve as an unknown flag and exits 2: so the report quoting
// that refusal is proof the flag reached the child, the log caught it, and the
// session would be told.
func TestSpawnFor_StartsThisExecutableWithServeIntoServeLog(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	var cfg config.Config
	spawn := spawnFor(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if spawn == nil {
		t.Skip("this host has a service config, so no frontend here may spawn")
	}
	sp, err := spawn()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "autodb", "serve.log"); sp.LogPath != want {
		t.Errorf("LogPath %q, want %q", sp.LogPath, want)
	}
	waitExited(t, sp)
	if got := sp.Report(); !strings.HasPrefix(got, "exit status 2: ") || !strings.Contains(got, "-serve") {
		t.Errorf("Report() = %q; want the test binary's refusal of -serve, exit status 2", got)
	}
}

// THE BUG, END TO END THROUGH THE REAL BINARY: the store is held by a process
// with no address to attach to, so the session spawns a server, and that
// server refuses. The session used to dial for its whole probe window and then
// say only that nothing answered. It now reports the refusal as soon as the
// server exits, and the refusal names the holder.
func TestSpawn_ARefusedServeIsReportedAtOnceAndNamesTheHolder(t *testing.T) {
	e := newDiscoveryEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Hold the store the way a migration does: the lease, and no address.
	mcfg := config.Meta{Engine: "sqlite", Path: filepath.Join(e.data, "autodb", "meta.db")}
	if err := os.MkdirAll(filepath.Dir(mcfg.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := meta.Open(ctx, mcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lease, err := meta.AcquireLease(ctx, store, mcfg, meta.LeaseHolder{Role: "migrate"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()

	run := runDir(t)
	ep := config.Endpoint{Network: "unix", Address: filepath.Join(run, "autodb.sock")}
	logPath := filepath.Join(e.data, "serve.log")
	s := tuiapp.NewSessionOn(ep.Network, ep.Address, logger.Nop{}, nil)
	s.SetSpawner(func() (tuiapp.Spawned, error) { return startLogged(e.cmd(run, "--serve"), logPath) })
	s.SetLocator(func(ctx context.Context) (rpc.Located, error) { return rpc.Locate(ctx, ep, mcfg) })

	began := time.Now()
	_, err = s.Connect(ctx)
	took := time.Since(began)
	if err == nil {
		s.Close()
		t.Fatal("Connect succeeded against a store another process holds")
	}
	msg := err.Error()
	t.Logf("Connect reported, after %s: %s", took.Round(time.Millisecond), msg)
	for _, want := range []string{
		"exited without serving",
		fmt.Sprintf("exit status %d", exitAlreadyServing),
		fmt.Sprintf("held by autodb (migrate), pid %d", os.Getpid()),
		"full log: " + logPath,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q:\n%s", want, msg)
		}
	}
	// Well inside the 15s probe window: the exit ended the wait, not the clock.
	if took > 10*time.Second {
		t.Errorf("Connect took %s; the server's exit should have ended it", took)
	}
}

// --serve SERVES ON THIS PLATFORM: a daemon started as a frontend would start
// it answers on its socket, takes its first administrator, and runs a query.
// On darwin this is the cell the lease's self-refusal failed: every --serve
// refused itself before it answered anything.
func TestServe_ADaemonRunsAQueryOverItsSocket(t *testing.T) {
	e := newDiscoveryEnv(t)
	run := runDir(t)
	e.serve(t, run)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli := dialSession(ctx, t, filepath.Join(run, "autodb.sock"))

	res, err := cli.Call(ctx, "auth.bootstrap", "root", "a-long-enough-passphrase")
	if err != nil {
		t.Fatalf("auth.bootstrap: %v", err)
	}
	token, _ := res.(map[string]any)["token"].(string)
	if token == "" {
		t.Fatalf("auth.bootstrap answered %#v", res)
	}
	id, err := cli.Call(ctx, "conn.create", token, "target", "sqlite", "file:servesmoke?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("conn.create: %v", err)
	}
	res, err = cli.Call(ctx, "exec.run", token, id, "SELECT 41 + 1 AS answer")
	if err != nil {
		t.Fatalf("exec.run: %v", err)
	}
	rows, _ := res.(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("exec.run answered %#v, want one row", res)
	}
	if row, _ := rows[0].([]any); len(row) != 1 || fmt.Sprint(row[0]) != "42" {
		t.Errorf("exec.run row %#v, want [42]", rows[0])
	}
}

// dialSession dials a daemon's unix socket and completes the handshake.
func dialSession(ctx context.Context, t *testing.T, sock string) *golibrpc.Client {
	t.Helper()
	cli, err := golibrpc.Dial(ctx, sock, msgpackrpc.New(nil), golibrpc.ClientNetwork("unix"))
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol, "name": "serve-smoke"}); err != nil {
		t.Fatalf("sys.hello: %v", err)
	}
	return cli
}
