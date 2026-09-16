// mutate runs the checked-in mutation controls.
//
// A CONTROL SET WITH NO RUNNER IS A CONTRACT NOBODY EXECUTES. The definitions
// were checked in so they could not drift from the code; without something that
// consumes them, every run was still improvised from a prompt — which is how a
// control came to name a deleted test, and how another was scored against an
// anchor that matched four rows instead of one. This is the piece that makes a
// ledger replayable: same input, same commands, same classification.
//
//	go run ./internal/gatemutation/cmd/mutate -root /path/to/disposable/copy
//
// It refuses to run against a tree it was not told is disposable, applies
// exactly one edit at a time, and restores before moving on. It exits non-zero
// if ANY control is GREEN or INVALID, because both mean a guarantee is not
// proven — a surviving mutant and an unrunnable control are different failures
// with the same consequence.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/internal/gatemutation"
)

// Verdict is what one control earned.
type Verdict string

const (
	// RED: the cell failed on its own assertion. The guarantee is proven.
	RED Verdict = "RED"
	// GREEN: the cell passed with the code broken. The guarantee is NOT proven.
	GREEN Verdict = "GREEN"
	// INVALID: nothing was proven either way — the tree would not build, the
	// anchor did not match once, the named cell did not run, or it timed out.
	// Deliberately distinct from GREEN: one says the test is weak, the other
	// says the run told us nothing, and conflating them hides which.
	INVALID Verdict = "INVALID"
)

const defaultTimeout = 120 * time.Second

func main() {
	root := flag.String("root", "", "disposable copy of the repository to mutate (required)")
	only := flag.String("only", "", "run just this control, by name")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "mutate: -root is required, and it must be a copy you can afford to lose")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(2)
	}

	controls := gatemutation.All()
	var failures int
	for _, m := range controls {
		if *only != "" && m.Name != *only {
			continue
		}
		v, detail := run(abs, m)
		fmt.Printf("%s=%s\n", m.Name, v)
		if detail != "" {
			fmt.Println(indent(detail))
		}
		if v != RED {
			failures++
			fmt.Printf("  UNPROVEN: %s\n", m.Guarantee)
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\nmutate: %d control(s) did not earn RED; see UNPROVEN above\n", failures)
		os.Exit(1)
	}
}

// run applies one control and classifies what happened.
func run(root string, m gatemutation.Mutation) (Verdict, string) {
	path := filepath.Join(root, m.File)
	original, err := os.ReadFile(path)
	if err != nil {
		return INVALID, "cannot read " + m.File + ": " + err.Error()
	}
	// ALWAYS RESTORED, even on a panic further down: a mutated tree left
	// behind poisons every later control and every gate run that follows.
	defer os.WriteFile(path, original, 0o644)

	src := string(original)
	if n := strings.Count(src, m.Anchor); n != 1 {
		return INVALID, fmt.Sprintf("the anchor matches %d times in %s, so this control would "+
			"change something it never intended (or nothing at all)", n, m.File)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(src, m.Anchor, m.Replacement, 1)), 0o644); err != nil {
		return INVALID, "cannot write " + m.File + ": " + err.Error()
	}

	// BUILD FIRST, so nothing is ever scored on a tree that does not compile.
	if out, err := goRun(root, defaultTimeout, "build", "./..."); err != nil {
		return INVALID, "the mutated tree does not build, so the cell was never reached:\n" + out
	}

	timeout := m.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	args := []string{"test", m.Package, "-run", "^" + m.Test + "$", "-count=1", "-v",
		"-timeout", timeout.String()}
	if m.Count > 1 {
		args[4] = fmt.Sprintf("-count=%d", m.Count)
	}
	out, err := goRun(root, timeout+30*time.Second, args...)

	// THE CELL MUST HAVE ACTUALLY RUN. A -run that matches nothing exits 0 and
	// reads exactly like a passing test, which would score a surviving mutant
	// as proof.
	if !strings.Contains(out, "=== RUN   "+m.Test) {
		return INVALID, fmt.Sprintf("%s never ran in %s; the control proves nothing:\n%s",
			m.Test, m.Package, out)
	}
	if strings.Contains(out, "panic: test timed out") {
		return INVALID, "the cell timed out rather than failing on its assertion:\n" + out
	}
	if err == nil {
		return GREEN, "the cell passed with the code broken:\n" + out
	}
	if !strings.Contains(out, "--- FAIL: "+m.Test) {
		return INVALID, "the run failed, but not on the named cell's assertion:\n" + out
	}
	return RED, firstFailure(out)
}

func goRun(dir string, bound time.Duration, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	// A DISPOSABLE COPY HAS NO .git, and the toolchain stamps VCS status by
	// default -- so every build in it fails for a reason that has nothing to
	// do with the mutation. Without this the runner scores INVALID for every
	// control, which is honest and useless.
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(bound):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return string(out), errors.New("timed out")
	}
}

// firstFailure keeps the assertion and drops the rest, because a verdict is
// read far more often than a full log.
func firstFailure(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--- FAIL") || (strings.Contains(t, ".go:") && strings.Contains(t, ":")) {
			keep = append(keep, t)
		}
		if len(keep) >= 4 {
			break
		}
	}
	return strings.Join(keep, "\n")
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
