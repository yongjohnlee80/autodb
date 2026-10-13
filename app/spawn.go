package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// A SPAWNED SERVER THAT REFUSES IS REPORTED IN ITS OWN WORDS.
//
// The server a frontend spawns writes to serve.log, never to the terminal. When
// it refused to start (another autodb holds the store, the config is invalid)
// the frontend used to keep dialing a socket nobody would ever bind, and after
// the whole probe window said only "server did not answer within 15s". The
// reason sat in serve.log, which on macOS is where the lease's self-refusal
// hid. startLogged watches the child's exit, and the session reports the
// child's reason as soon as it is gone.
//
// THE REASON COMES FROM A FILE ONLY THAT CHILD WRITES, never from serve.log.
// serve.log is shared: two frontends that both find no daemon spawn two
// servers at once (the bind and lease race Connect already handles), and both
// append there. No offset or tail of it says which lines are whose. So each
// spawn gets its own report file, named to the child in $AUTODB_SPAWN_REPORT:
//
//   - the child writes what it printed when it refused (spawnReport.refused);
//   - the child removes the file once it serves, since there is nothing to
//     report and a later failure is not this spawn's answer
//     (spawnReport.serving);
//   - the parent reads it when the child exits, and removes it.
//
// serve.log is untouched by this: it stays the one stable path where an
// operator reads every server's whole output. The child's stdio is a file,
// never a pipe to the frontend, because the server outlives the frontend and a
// pipe would break under it when the frontend exits.

// spawnReportEnv names the child's report file.
const spawnReportEnv = "AUTODB_SPAWN_REPORT"

// spawnReportPattern is the report files' name, in the log's directory.
const spawnReportPattern = ".spawn-*.report"

// staleSpawnReport is the age past which a report file is debris: its spawner
// exited before its child did, so nobody will read it.
const staleSpawnReport = time.Hour

// reportBytes and reportLines bound what a report quotes: enough for a
// refusal, which is a line or three, and short enough for a status line.
const (
	reportBytes = 4 << 10
	reportLines = 6
)

// serveLogPath is the spawned server's log: $XDG_STATE_HOME/autodb/serve.log,
// or ~/.local/state/autodb/serve.log. The directory is created 0700.
func serveLogPath() (string, error) {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	logDir := filepath.Join(stateDir, "autodb")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(logDir, "serve.log"), nil
}

// startLogged starts cmd detached (its own session, no stdin) with stdout and
// stderr appended to logPath, and reports its exit and the reason it wrote to
// its own report file.
func startLogged(cmd *exec.Cmd, logPath string) (tuiapp.Spawned, error) {
	dir := filepath.Dir(logPath)
	sweepSpawnReports(dir, time.Now())
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return tuiapp.Spawned{}, err
	}
	defer logFile.Close()
	rf, err := os.CreateTemp(dir, spawnReportPattern)
	if err != nil {
		return tuiapp.Spawned{}, err
	}
	reportPath := rf.Name()
	_ = rf.Close()

	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, spawnReportEnv+"="+reportPath)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(reportPath)
		return tuiapp.Spawned{}, err
	}
	// Reap the child when it exits. A server normally outlives this process;
	// one that exits refused or failed, and Exited tells the session so.
	exited := make(chan struct{})
	var waitErr error
	var said string
	go func() {
		waitErr = cmd.Wait()
		said = readSpawnReport(reportPath)
		close(exited)
	}()
	return tuiapp.Spawned{
		LogPath: logPath,
		Exited:  exited,
		Report: func() string {
			<-exited // already closed when called; also orders the reads above
			return exitReport(waitErr, said)
		},
	}, nil
}

// readSpawnReport reads and removes a child's report file, onto one line: at
// most reportLines non-empty lines from its last reportBytes. A file the child
// removed (it served) or never wrote adds nothing.
func readSpawnReport(path string) string {
	b, _ := os.ReadFile(path)
	_ = os.Remove(path)
	clipped := len(b) > reportBytes
	if clipped {
		b = b[len(b)-reportBytes:]
	}
	lines := strings.Split(string(b), "\n")
	if clipped {
		lines = lines[1:] // the first line was cut by the byte bound
	}
	var kept []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > reportLines {
		kept = kept[len(kept)-reportLines:]
	}
	return strings.Join(kept, " | ")
}

// sweepSpawnReports removes report files older than staleSpawnReport: a
// spawner that exits before its child leaves the file, and nothing else would.
func sweepSpawnReports(dir string, now time.Time) {
	stale, _ := filepath.Glob(filepath.Join(dir, spawnReportPattern))
	for _, p := range stale {
		if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) > staleSpawnReport {
			_ = os.Remove(p)
		}
	}
}

// exitReport is how the child exited, then its reason.
func exitReport(waitErr error, said string) string {
	how := "exit status 0"
	if waitErr != nil {
		how = waitErr.Error() // "exit status 69", "signal: killed"
	}
	if said == "" {
		return how
	}
	return how + ": " + said
}

// spawnReport is the child's side: the report file its spawner named, or
// nothing for a server nobody spawned.
type spawnReport struct{ path string }

// takeSpawnReport reads the report path from the environment and removes it
// there, so nothing this process starts inherits a file that is not its own.
func takeSpawnReport() spawnReport {
	p := os.Getenv(spawnReportEnv)
	_ = os.Unsetenv(spawnReportEnv)
	return spawnReport{path: p}
}

// serving withdraws the report: this server is up, so there is no refusal to
// give, and a failure much later is not the answer its spawner waited for.
func (r spawnReport) serving() {
	if r.path != "" {
		_ = os.Remove(r.path)
	}
}

// refused writes what this server printed when it refused, for its spawner.
//
// Only into a file that still exists: the spawner created it, and a missing
// one means the spawner swept it or the server already served. Creating it
// again would leave a file nobody reads.
func (r spawnReport) refused(said string) {
	if r.path == "" {
		return
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	_, _ = f.WriteString(said)
	_ = f.Close()
}
