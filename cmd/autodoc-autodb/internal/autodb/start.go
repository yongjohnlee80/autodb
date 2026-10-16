package autodb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Endpoint is where `autodb --print-endpoint` says its server listens, and the store it must
// serve: an id to compare with the hello's, "pending" for a store not created yet (no server can
// be serving it), or "" when there is nothing to check.
type Endpoint struct {
	Network, Addr, Store string
}

// printTimeout bounds one --print-endpoint.
//
// NOTE: a deadline: the command reads the config and the store's lease and exits; one that hangs
// announces nothing to wait on.
const printTimeout = 10 * time.Second

// PrintEndpoint runs bin --print-endpoint.
func PrintEndpoint(ctx context.Context, bin string) (Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, printTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "--print-endpoint")
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		why := firstLine(errOut.String())
		if why == "" {
			why = err.Error()
		}
		return Endpoint{}, fmt.Errorf("autodb --print-endpoint: %s", why)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	network, addr, ok := strings.Cut(lines[0], "\t")
	if !ok || network == "" || addr == "" {
		return Endpoint{}, fmt.Errorf("autodb --print-endpoint printed %q, not <network>\\t<address>", firstLine(out.String()))
	}
	ep := Endpoint{Network: network, Addr: addr}
	if len(lines) > 1 {
		if key, val, ok := strings.Cut(lines[1], "\t"); ok && key == "store" {
			ep.Store = val
		}
	}
	return ep, nil
}

// exitAlreadyServing is `autodb --serve`'s exit when another AutoDB already holds the endpoint
// or the store: a start that lost a race to another, which then answers.
const exitAlreadyServing = 69

// lostRace reports whether a started server's exit code says another server won the start.
func lostRace(code int) bool { return code == exitAlreadyServing }

// DefaultWindow is how long Start keeps dialing after the first failure.
const DefaultWindow = 15 * time.Second

// Starter reaches AutoDB's server, starting it when nothing answers.
type Starter struct {
	Bin           string
	ClientVersion string
	// Window bounds the dialing after the first failure; DefaultWindow when 0.
	Window time.Duration
	// LogPath is the started server's log; $XDG_STATE_HOME/autodb/serve.log when "".
	LogPath string
}

// Start dials the endpoint --print-endpoint names. When nothing answers it starts
// `autodb --serve` detached, once, and dials again until the window ends. A start that exits 69
// lost a race to another server, which is then dialed; any other exit is reported in the
// server's words.
func (s Starter) Start(ctx context.Context) (*Client, error) {
	window := s.Window
	if window <= 0 {
		window = DefaultWindow
	}
	var (
		deadline time.Time
		started  *spawned
		exited   <-chan struct{}
		gone     bool
		last     error
		backoff  = 100 * time.Millisecond
	)
	for {
		ep, err := PrintEndpoint(ctx, s.Bin)
		if err != nil {
			return nil, err
		}
		if ep.Store == "pending" {
			last = errors.New("the store does not exist yet")
		} else {
			c, err := Dial(ctx, ep.Network, ep.Addr, s.ClientVersion)
			if err == nil {
				if ep.Store != "" && c.Hello.StoreID != ep.Store {
					_ = c.Close()
					return nil, fmt.Errorf("the AutoDB answering at %s serves another store", ep.Addr)
				}
				return c, nil
			}
			var de *DialError
			if !errors.As(err, &de) {
				return nil, err
			}
			last = err
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(window)
		}
		if gone {
			return nil, started.exitedError()
		}
		if started == nil {
			if started, err = spawn(s.Bin, s.LogPath); err != nil {
				return nil, fmt.Errorf("starting autodb --serve: %w", err)
			}
			exited = started.exited
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("AutoDB did not answer within %s (last: %v); its log is %s", window, last, started.logPath)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-exited:
			timer.Stop()
			exited = nil
			// the other server won and answers: keep dialing it
			if !lostRace(started.code) {
				gone = true // one more dial, then its own words
			}
		// NOTE: backoff. A server that is still starting cannot announce itself to a client with no
		// connection to it; its exit, the one event it can produce, is waited on above.
		case <-timer.C:
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

// spawned is a started `autodb --serve`: its exit, when it exits, and what it said.
type spawned struct {
	logPath string
	exited  chan struct{}
	code    int    // its exit code, once exited
	report  string // what it wrote to its report file, once exited
}

func (s *spawned) exitedError() error {
	msg := fmt.Sprintf("autodb --serve exited (status %d) without serving", s.code)
	if s.report != "" {
		msg += ": " + s.report
	}
	return errors.New(msg + "; its log is " + s.logPath)
}

// spawnReportEnv names the file a started server writes its refusal to, and removes once it
// serves.
const spawnReportEnv = "AUTODB_SPAWN_REPORT"

// spawn starts bin --serve in a session of its own, its output appended to logPath.
func spawn(bin, logPath string) (*spawned, error) {
	if logPath == "" {
		var err error
		if logPath, err = serveLogPath(); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	rf, err := os.CreateTemp(filepath.Dir(logPath), ".spawn-*.report")
	if err != nil {
		return nil, err
	}
	reportPath := rf.Name()
	_ = rf.Close()

	cmd := exec.Command(bin, "--serve")
	cmd.Env = append(os.Environ(), spawnReportEnv+"="+reportPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(reportPath)
		return nil, err
	}
	sp := &spawned{logPath: logPath, exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			sp.code = 0
		case errors.As(err, &ee):
			sp.code = ee.ExitCode()
		default:
			sp.code = -1
		}
		b, _ := os.ReadFile(reportPath)
		_ = os.Remove(reportPath)
		sp.report = strings.Join(strings.Fields(firstLine(string(b))), " ")
		close(sp.exited)
	}()
	return sp, nil
}

// serveLogPath is $XDG_STATE_HOME/autodb/serve.log (or ~/.local/state/autodb/serve.log), where
// AutoDB's own frontends log the server they start.
func serveLogPath() (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(state, "autodb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "serve.log"), nil
}
