package app

import (
	"context"
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

// reportingChild is a stand-in for `autodb --serve`: it writes noise to its
// stdout (serve.log), then its refusal into the report file its spawner named,
// and exits 69. It waits for gate to exist first, when gate is not empty, so a
// cell can release several children at once.
func reportingChild(name, gate string) *exec.Cmd {
	script := `
if [ -n "$GATE" ]; then while [ ! -e "$GATE" ]; do sleep 0.01; done; fi
i=0; while [ $i -lt 40 ]; do echo "$NAME noise $i"; i=$((i+1)); done
echo "$NAME refusing to serve: held by pid 7" >&2
printf 'autodb: %s refusing to serve: held by pid 7\n' "$NAME" > "$AUTODB_SPAWN_REPORT"
exit 69`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "NAME="+name, "GATE="+gate)
	return cmd
}

// THE REPORT IS THE CHILD'S OWN: how it exited and the reason it wrote, not
// what it or any server logged. The log keeps everything, appended.
func TestStartLogged_ReportsTheExitAndTheChildsOwnReason(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "serve.log")
	if err := os.WriteFile(logPath, []byte("an earlier server said this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := startLogged(reportingChild("A", ""), logPath)
	if err != nil {
		t.Fatal(err)
	}
	if sp.LogPath != logPath {
		t.Errorf("LogPath %q, want %q", sp.LogPath, logPath)
	}
	waitExited(t, sp)
	want := "exit status 69: autodb: A refusing to serve: held by pid 7"
	if got := sp.Report(); got != want {
		t.Errorf("Report() = %q, want %q", got, want)
	}
	b, _ := os.ReadFile(logPath)
	if !strings.HasPrefix(string(b), "an earlier server said this\n") || !strings.Contains(string(b), "A noise 39") {
		t.Errorf("the log lost what was in it, or what the child wrote:\n%s", b)
	}
	// The report file is read once and removed.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(logPath), spawnReportPattern)); len(left) != 0 {
		t.Errorf("report files left behind: %v", left)
	}
}

// TWO SPAWNS AT ONCE, ONE SHARED LOG: two frontends that find no daemon spawn
// two servers together, and both append to serve.log, interleaved. Each report
// must be its own child's refusal and nothing of the other's. A report taken
// from the log by offset reads the other child's lines as its own.
func TestStartLogged_ConcurrentSpawnsReportOnlyTheirOwnChild(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serve.log")
	gate := filepath.Join(dir, "go")
	a, err := startLogged(reportingChild("A", gate), logPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := startLogged(reportingChild("B", gate), logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitExited(t, a)
	waitExited(t, b)
	for _, c := range []struct {
		name, other string
		sp          tuiapp.Spawned
	}{{"A", "B", a}, {"B", "A", b}} {
		got := c.sp.Report()
		if want := "exit status 69: autodb: " + c.name + " refusing to serve: held by pid 7"; got != want {
			t.Errorf("%s's report = %q, want %q", c.name, got, want)
		}
		if strings.Contains(got, c.other+" ") {
			t.Errorf("%s's report quotes %s: %q", c.name, c.other, got)
		}
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"A noise 39", "B noise 39", "A refusing", "B refusing"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("serve.log lacks %q: the operator's log must keep both children's output", want)
		}
	}
}

// A child that exits cleanly and writes no reason is still reported as an exit.
func TestStartLogged_ASilentExitSaysHowItExited(t *testing.T) {
	sp, err := startLogged(exec.Command("/bin/sh", "-c", "echo only in the log; exit 0"), filepath.Join(t.TempDir(), "serve.log"))
	if err != nil {
		t.Fatal(err)
	}
	waitExited(t, sp)
	if got := sp.Report(); got != "exit status 0" {
		t.Errorf("Report() = %q, want %q", got, "exit status 0")
	}
}

// A log that cannot be opened, or a child that cannot start, is an error from
// the spawn rather than a session left dialing, and leaves no report file.
func TestStartLogged_FailuresToStartAreErrors(t *testing.T) {
	if _, err := startLogged(exec.Command("/bin/true"), filepath.Join(t.TempDir(), "missing", "serve.log")); err == nil {
		t.Error("an unopenable log was accepted")
	}
	dir := t.TempDir()
	if _, err := startLogged(exec.Command(filepath.Join(dir, "no-such-binary")), filepath.Join(dir, "serve.log")); err == nil {
		t.Error("a binary that does not exist was started")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, spawnReportPattern)); len(left) != 0 {
		t.Errorf("a failed start left report files: %v", left)
	}
}

func TestReadSpawnReport_Bounds(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The last reportLines non-empty lines, trimmed; the file is removed.
	var many strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&many, "  line %d  \n\n", i)
	}
	p := write("many", many.String())
	if got, want := readSpawnReport(p), "line 5 | line 6 | line 7 | line 8 | line 9 | line 10"; got != want {
		t.Errorf("many lines: %q, want %q", got, want)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the report was not removed after reading: %v", err)
	}

	// Past the byte bound, the line the bound cut is dropped, not quoted half.
	long := strings.Repeat("x", reportBytes) + "\nlast\n"
	if got := readSpawnReport(write("long", long)); got != "last" {
		t.Errorf("clipped: %q, want %q", got, "last")
	}
	// A report the child removed (it served) adds nothing.
	if got := readSpawnReport(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("absent report: %q, want empty", got)
	}
}

// A spawner that exits before its child leaves the report file; the next
// spawn removes it once it is stale, and nothing younger.
func TestSweepSpawnReports_RemovesOnlyStaleOnes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".spawn-old.report")
	young := filepath.Join(dir, ".spawn-young.report")
	other := filepath.Join(dir, "serve.log")
	for _, p := range []string{old, young, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	stale := now.Add(-2 * staleSpawnReport)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	sweepSpawnReports(dir, now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a stale report survived the sweep: %v", err)
	}
	for _, p := range []string{young, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the sweep removed %s: %v", p, err)
		}
	}
}

// THE CHILD'S SIDE: it takes the path out of its environment, writes only into
// a file its spawner made, and withdraws the file once it serves.
func TestSpawnReport_TheChildsSide(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".spawn-x.report")
	if err := os.WriteFile(p, []byte("stale words"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spawnReportEnv, p)
	r := takeSpawnReport()
	if _, set := os.LookupEnv(spawnReportEnv); set || r.path != p {
		t.Fatalf("took %q, env still set: %v", r.path, set)
	}
	r.refused("autodb: refusing\n")
	if b, _ := os.ReadFile(p); string(b) != "autodb: refusing\n" {
		t.Errorf("report holds %q, want only the refusal", b)
	}
	r.serving()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("serving left the report: %v", err)
	}
	r.refused("autodb: much later\n") // the file is gone: never recreated
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("a refusal after serving recreated the report: %v", err)
	}
	// A server nobody spawned has no report, and both calls are no-ops.
	none := takeSpawnReport()
	none.refused("x")
	none.serving()
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
// which refuses --serve as an unknown flag, prints its usage and exits 2: the
// refusal in serve.log is proof the flag reached the child and the log caught
// its output, and the exit status in the report is proof the session is told.
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
	// The test binary knows nothing of the report file, so the report is the
	// exit alone; what it printed is in serve.log.
	if got := sp.Report(); got != "exit status 2" {
		t.Errorf("Report() = %q; want the test binary's exit status 2", got)
	}
	if b, _ := os.ReadFile(sp.LogPath); !strings.Contains(string(b), "flag provided but not defined: -serve") {
		t.Errorf("serve.log does not hold the child's refusal of -serve:\n%s", b)
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

// A SPAWNED SERVER THAT SERVES WITHDRAWS ITS REPORT: there is no refusal to
// give, and when it stops much later its exit is nobody's answer. The real
// binary, through startLogged.
func TestSpawn_AServingChildWithdrawsItsReport(t *testing.T) {
	e := newDiscoveryEnv(t)
	run := runDir(t)
	logPath := filepath.Join(e.data, "serve.log")
	sp, err := startLogged(e.cmd(run, "--serve"), logPath)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(run, "autodb.sock")
	var hello rpc.Hello
	for deadline := time.Now().Add(30 * time.Second); ; {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		hello, err = rpc.ProbeHello(ctx, "unix", sock)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the spawned daemon never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// It announces, withdraws the report, then serves: an answered hello
	// means the withdrawal has happened.
	if left, _ := filepath.Glob(filepath.Join(e.data, spawnReportPattern)); len(left) != 0 {
		t.Errorf("a serving daemon left its report file: %v", left)
	}
	if err := syscall.Kill(int(hello.PID), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitExited(t, sp)
	if got := sp.Report(); strings.Contains(got, ": ") {
		t.Errorf("a daemon that served and was stopped reported a reason: %q", got)
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
