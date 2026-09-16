package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CONTAINMENT IS PROVED BY BUILDING SOMETHING THAT ESCAPES A LESSER ONE.
//
// Killing only the parent leaves the test binary alive holding the output pipe,
// and CombinedOutput does not return until that pipe closes — so the runner
// waits forever on a bound it believes it enforced. That is the worst place to
// hang: the code that runs precisely when something has already gone wrong.
// Inspection cannot tell a group kill from a parent kill; both compile and both
// look right.
//
// COMPILATION HAPPENS BEFORE THE CLOCK STARTS, AND ABSENCE IS NEVER A SKIP.
// The first version began the five-second bound before building the helper, so
// on a cold cache the compile could consume the whole window: the timeout
// fired, no descendant had ever started, the pid file was missing, and the cell
// SKIPPED — green, having proved nothing, on exactly the machine where it
// matters. A cell that can pass without constructing its own failure is not a
// cell. So the helper is compiled first, outside the measured window, and a
// missing pid is fatal.
func TestRunBounded_KillsTheWholeProcessTree(t *testing.T) {
	helper := buildHolder(t)
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")

	const bound = 5 * time.Second
	cmd := exec.Command(helper, "-test.run", "^TestHolds$", "-test.timeout", "9m")
	cmd.Env = append(os.Environ(), "HOLDER_PID_FILE="+pidFile)

	start := time.Now()
	out, err := runBounded(cmd, bound)
	elapsed := time.Since(start)

	// THE DESCENDANT MUST HAVE EXISTED. Without this the cell passes when the
	// helper never ran, which is the vacuous case it was rewritten to remove.
	pid := waitForPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err == nil {
		t.Fatalf("runBounded returned success for a helper that blocks for ten minutes:\n%s", out)
	}
	if !strings.Contains(err.Error(), "containment timeout") {
		t.Errorf("classified as %v, want a containment timeout", err)
	}
	if ceiling := bound + 20*time.Second; elapsed > ceiling {
		t.Errorf("runBounded took %s against a %s bound; containment did not contain, which "+
			"is a hang in the code that runs when something has already gone wrong",
			elapsed, bound)
	}

	// AND IT MUST BE GONE. A parent-only kill leaves it running, and one leaked
	// process per control is how a gate machine degrades over a long run.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // reaped, as required
		}
		if err != nil && err != syscall.EPERM {
			t.Fatalf("checking descendant %d: %v", pid, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("descendant %d survived the containment timeout; killing the parent alone leaves "+
		"the test binary and its children holding the output pipe", pid)
}

// buildHolder compiles the helper OUTSIDE the measured window.
func buildHolder(t *testing.T) string {
	t.Helper()
	mod := t.TempDir()
	write(t, filepath.Join(mod, "go.mod"), "module holder\n\ngo 1.25\n")
	write(t, filepath.Join(mod, "holder_test.go"), `package holder

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestHolds starts a child that inherits stdout, so the pipe stays open even
// when the parent is killed — which is the whole point.
func TestHolds(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("HOLDER_PID_FILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	}
	time.Sleep(10 * time.Minute)
}
`)
	bin := filepath.Join(t.TempDir(), "holder.test")
	build := exec.Command("go", "test", "-c", "-o", bin, ".")
	build.Dir = mod
	build.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compiling the holder helper: %v\n%s", err, out)
	}
	return bin
}

// waitForPID requires the descendant to have announced itself.
func waitForPID(t *testing.T, path string, bound time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil {
			if n, cerr := strconv.Atoi(strings.TrimSpace(string(body))); cerr == nil && n > 0 {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the helper never recorded a descendant pid at %s; this cell cannot prove "+
		"containment without one, and passing anyway is how it becomes scenery", path)
	return 0
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = fmt.Sprintf
