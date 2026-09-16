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
// Killing only the `go` process leaves the test binary alive, holding the
// output pipe — and CombinedOutput does not return until that pipe closes, so
// the runner waits forever on a bound it believes it enforced. That is the
// worst possible place for a hang: the evidence runner stops, silently, in the
// path that exists for when something has already gone wrong.
//
// Inspection cannot tell a group kill from a parent kill; both compile and both
// look right. So this cell spawns a descendant that inherits stdout and blocks,
// and asserts three things a parent-only kill cannot satisfy: the call returns
// inside the bound, it classifies as a timeout, and the descendant is gone.
func TestGoRun_ContainmentKillsTheWholeProcessTree(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	mod := t.TempDir()
	pidFile := filepath.Join(mod, "descendant.pid")

	// A module whose test starts a child that inherits stdout and blocks. The
	// child records its own pid so the cell can prove it was reaped.
	write(t, filepath.Join(mod, "go.mod"), "module holder\n\ngo 1.25\n")
	write(t, filepath.Join(mod, "holder_test.go"), fmt.Sprintf(`package holder

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestHolds(t *testing.T) {
	// sleep inherits this process's stdout, so the pipe stays open even if the
	// go parent is killed.
	cmd := exec.Command("sleep", "600")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(%q, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	time.Sleep(10 * time.Minute)
}
`, pidFile))

	const bound = 5 * time.Second
	start := time.Now()
	out, err := goRun(mod, bound, "test", "./...", "-run", "^TestHolds$", "-count=1", "-timeout", "9m")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("goRun returned success for a test that blocks for ten minutes:\n%s", out)
	}
	// The reap path allows ten seconds beyond the bound; anything past that is
	// the hang this cell exists to catch.
	if ceiling := bound + 20*time.Second; elapsed > ceiling {
		t.Errorf("goRun took %s against a %s bound; containment did not contain, which is a "+
			"hang in the code that runs when something has already gone wrong", elapsed, bound)
	}
	if !strings.Contains(err.Error(), "containment timeout") {
		t.Errorf("classified as %v, want a containment timeout", err)
	}

	// THE DESCENDANT MUST BE GONE. A parent-only kill leaves it running, and a
	// leaked process per control is how a gate machine degrades over a run.
	pid := readPID(t, pidFile)
	if pid == 0 {
		t.Skip("the descendant never recorded its pid; nothing to assert about reaping")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone, as required
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak it from the cell either
	t.Errorf("descendant %d survived the containment timeout; killing the go parent alone "+
		"leaves the test binary and its children holding the output pipe", pid)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return 0
	}
	return n
}

var _ = exec.Command
