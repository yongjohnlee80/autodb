package app

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// A SPAWNED SERVER THAT REFUSES IS REPORTED IN ITS OWN WORDS.
//
// The server a frontend spawns writes to serve.log, never to the terminal. When
// it refused to start (another autodb holds the store, the config is invalid)
// the frontend used to keep dialing a socket nobody would ever bind, and after
// the whole probe window said only "server did not answer within 15s". The
// reason sat in serve.log, which on macOS is where the lease's self-refusal
// hid. startLogged watches the child's exit, and the session reports what the
// child wrote as soon as it is gone.

// reportTailBytes and reportTailLines bound what a report quotes from the log:
// enough for a refusal, which is a line or three, and short enough for a
// status line.
const (
	reportTailBytes = 4 << 10
	reportTailLines = 6
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
// stderr appended to logPath, and reports its exit and what it wrote.
//
// The log is shared and append-only, so the report reads only from where this
// child's output begins: what an earlier server wrote is not this one's reason.
func startLogged(cmd *exec.Cmd, logPath string) (tuiapp.Spawned, error) {
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return tuiapp.Spawned{}, err
	}
	defer logFile.Close()
	from, err := logFile.Seek(0, io.SeekEnd)
	if err != nil {
		return tuiapp.Spawned{}, err
	}
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return tuiapp.Spawned{}, err
	}
	// Reap the child when it exits. A server normally outlives this process;
	// one that exits refused or failed, and Exited tells the session so.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	return tuiapp.Spawned{
		LogPath: logPath,
		Exited:  exited,
		Report: func() string {
			<-exited // already closed when called; also orders the read of waitErr
			return exitReport(waitErr, logTail(logPath, from))
		},
	}, nil
}

// exitReport is how the child exited, then the last of what it wrote.
func exitReport(waitErr error, tail string) string {
	how := "exit status 0"
	if waitErr != nil {
		how = waitErr.Error() // "exit status 69", "signal: killed"
	}
	if tail == "" {
		return how
	}
	return how + ": " + tail
}

// logTail is the last non-empty lines written to path from offset from, joined
// onto one line: at most reportTailLines lines from the last reportTailBytes.
// A log shorter than from was truncated or replaced under us; then all of it
// is this child's. An unreadable log adds nothing.
func logTail(path string, from int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()
	if from > size {
		from = 0
	}
	clipped := false
	if size-from > reportTailBytes {
		from, clipped = size-reportTailBytes, true
	}
	b := make([]byte, size-from)
	n, _ := f.ReadAt(b, from) // a short read quotes what was read
	lines := strings.Split(string(b[:n]), "\n")
	if clipped {
		lines = lines[1:] // the first line was cut by the byte bound
	}
	var kept []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > reportTailLines {
		kept = kept[len(kept)-reportTailLines:]
	}
	return strings.Join(kept, " | ")
}
