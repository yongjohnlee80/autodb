// mutate runs the checked-in mutation controls.
//
// A CONTROL SET WITH NO RUNNER IS A CONTRACT NOBODY EXECUTES. The definitions
// were checked in so they could not drift from the code; without something that
// consumes them, every run was improvised from a prompt — which is how a control
// came to name a deleted test, and how another was scored against an anchor
// matching four rows instead of one.
//
//	git archive HEAD | tar -x -C /tmp/disposable
//	go run ./internal/gatemutation/cmd/mutate -root /tmp/disposable
//
// EVERY CLASSIFICATION RULE HERE EXISTS BECAUSE ITS ABSENCE PRODUCED A FALSE
// VERDICT. The stock run guards against scoring a cell that was already red.
// The failure fingerprint guards against crediting a control for a neighbour's
// failure. The restoration check guards against one control poisoning the next.
// A timeout is never a verdict: a cell that detects a break by hanging is not a
// cell, it is a cell that has not been written yet.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/internal/gatemutation"
)

// Verdict is what one control earned.
type Verdict string

const (
	// RED: the cell failed on the assertion this control claims to provoke.
	RED Verdict = "RED"
	// GREEN: the cell passed with the code broken. The guarantee is NOT proven.
	GREEN Verdict = "GREEN"
	// INVALID: the run proved nothing either way. Kept distinct from GREEN
	// because "this test is weak" and "this run told us nothing" are different
	// problems, and conflating them hides which one you have.
	INVALID Verdict = "INVALID"
)

const containment = 120 * time.Second

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
	// A SOURCE WORKTREE IS NOT A MUTATION TARGET. Mutating one risks leaving
	// somebody's actual work broken if this process dies mid-control.
	if _, serr := os.Stat(filepath.Join(abs, ".git")); serr == nil {
		fmt.Fprintf(os.Stderr, "mutate: %s contains .git and looks like a source tree; "+
			"point -root at a disposable copy\n", abs)
		os.Exit(2)
	}

	controls := gatemutation.All()
	if *only != "" {
		// AN UNKNOWN NAME USED TO SELECT NOTHING AND EXIT 0 — a run that did
		// nothing, reported as success.
		var picked []gatemutation.Mutation
		for _, m := range controls {
			if m.Name == *only {
				picked = append(picked, m)
			}
		}
		if len(picked) != 1 {
			fmt.Fprintf(os.Stderr, "mutate: -only %q matches %d controls, want exactly 1\n",
				*only, len(picked))
			os.Exit(2)
		}
		controls = picked
	}

	tally := map[Verdict]int{}
	for _, m := range controls {
		v, detail := run(abs, m)
		tally[v]++
		fmt.Printf("%s=%s\n", m.Name, v)
		if detail != "" {
			fmt.Println(indent(detail))
		}
		if v != RED {
			fmt.Printf("  UNPROVEN: %s\n", m.Guarantee)
		}
		if v == INVALID {
			// A CONTROL THAT COULD NOT BE SCORED MAY HAVE LEFT THE TREE DIRTY,
			// and a poisoned tree turns every later verdict into fiction.
			fmt.Fprintln(os.Stderr, "mutate: stopping after an INVALID control; the tree can no "+
				"longer be trusted to be stock")
			report(tally, len(controls))
			os.Exit(1)
		}
	}

	executed := tally[RED] + tally[GREEN] + tally[INVALID]
	report(tally, len(controls))
	if executed != len(controls) {
		fmt.Fprintf(os.Stderr, "mutate: %d of %d controls executed\n", executed, len(controls))
		os.Exit(1)
	}
	if tally[RED] != len(controls) {
		os.Exit(1)
	}
}

func report(tally map[Verdict]int, total int) {
	fmt.Printf("\nRED=%d GREEN=%d INVALID=%d of %d\n",
		tally[RED], tally[GREEN], tally[INVALID], total)
}

// run applies one control and classifies what happened.
func run(root string, m gatemutation.Mutation) (Verdict, string) {
	path := filepath.Join(root, m.File)
	info, err := os.Stat(path)
	if err != nil {
		return INVALID, "cannot stat " + m.File + ": " + err.Error()
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return INVALID, "cannot read " + m.File + ": " + err.Error()
	}

	// THE STOCK CELL MUST BE GREEN BEFORE ANYTHING IS BROKEN.
	//
	// Without this a cell that was ALREADY failing at the submitted head earns
	// RED, and the control appears to prove a guarantee it never tested. That
	// is not hypothetical: it is precisely what happened to the matrix control,
	// whose cell was red from unrelated drift, and it is the reason this runner
	// exists at all.
	if out, v := exercise(root, m); v != GREEN {
		return INVALID, "the cell is not green before mutation, so nothing it does afterwards " +
			"can be attributed to the mutation:\n" + out
	}

	src := string(original)
	if n := strings.Count(src, m.Anchor); n != 1 {
		return INVALID, fmt.Sprintf("the anchor matches %d times in %s, so this control would "+
			"change something it never intended (or nothing at all)", n, m.File)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(src, m.Anchor, m.Replacement, 1)), info.Mode()); err != nil {
		return INVALID, "cannot write " + m.File + ": " + err.Error()
	}

	out, v := exercise(root, m)

	if rerr := restore(path, original, info.Mode()); rerr != nil {
		return INVALID, "the file could not be restored, so no later control can be trusted: " + rerr.Error()
	}

	switch v {
	case RED:
		return RED, firstFailure(out)
	case GREEN:
		return GREEN, "the cell passed with the code broken:\n" + out
	}
	return INVALID, out
}

// exercise runs the control's cell once and says what happened, without caring
// whether the tree is stock or mutated.
//
// GREEN here means "the cell passed"; RED means "it failed on the assertion
// this control claims". Everything else is INVALID.
func exercise(root string, m gatemutation.Mutation) (string, Verdict) {
	if out, err := goRun(root, containment, "build", "./..."); err != nil {
		return "the tree does not build, so the cell was never reached:\n" + out, INVALID
	}

	timeout := m.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	count := m.Count
	if count < 1 {
		count = 1
	}
	out, err := goRun(root, timeout+30*time.Second, "test", m.Package,
		"-run", "^"+m.Test+"$", fmt.Sprintf("-count=%d", count), "-v",
		"-timeout", timeout.String())

	// THE CELL MUST HAVE ACTUALLY RUN. A -run matching nothing exits 0 and
	// reads exactly like a passing test.
	if !strings.Contains(out, "=== RUN   "+m.Test) {
		return m.Test + " never ran in " + m.Package + ":\n" + out, INVALID
	}
	if strings.Contains(out, "panic: test timed out") {
		// A TIMEOUT IS NEVER A VERDICT. A cell that detects a break by hanging
		// has not been written yet; the runner's bound is containment, not the
		// oracle.
		return "the cell timed out rather than failing on its assertion:\n" + out, INVALID
	}
	if strings.Contains(out, "panic: ") {
		return "the cell panicked rather than failing on its assertion:\n" + out, INVALID
	}
	if err == nil {
		return out, GREEN
	}
	if !strings.Contains(out, "--- FAIL: "+m.Test) {
		return "the run failed, but not on the named cell:\n" + out, INVALID
	}
	// THE CLAIMED ASSERTION, NOT MERELY THE FUNCTION. Without this a
	// neighbouring subtest or an unrelated check credits the control for a
	// failure it did not cause.
	if m.Fails == "" {
		// NO CONTROL SCORES WITHOUT ONE. An optional fingerprint is a
		// permissive fallback, and the fallback is where the defect lives: a
		// control with none earns RED from any failure in the named cell,
		// including a neighbour's.
		return "the control declares no failure fingerprint, so RED would mean only that " +
			"something in " + m.Test + " failed:\n" + out, INVALID
	}
	if !strings.Contains(out, m.Fails) {
		return "the cell failed, but not on the assertion this control claims (" +
			m.Fails + "):\n" + out, INVALID
	}
	return out, RED
}

func goRun(dir string, bound time.Duration, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	// A disposable copy has no .git, and the toolchain stamps VCS status by
	// default, so every build in it fails for a reason unrelated to the
	// mutation.
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	return runBounded(cmd, bound)
}

// runBounded runs a command in its own process group and kills the whole group
// if it outlives the bound.
//
// SEPARATED FROM goRun SO IT CAN BE DRIVEN DIRECTLY. Containment is the one
// path that cannot be proved by reading -- a group kill and a parent kill both
// compile and both look right -- so the cell that proves it has to hand this
// function a command of its own, with nothing else inside the measured window.
func runBounded(cmd *exec.Cmd, bound time.Duration) (string, error) {
	// ITS OWN PROCESS GROUP, so a timeout can kill the test binary too.
	// Killing only the parent leaves the child holding the output pipe, and the
	// wait below never returns — a containment bound that does not contain.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// STARTED EXPLICITLY, AND THE PID IS TAKEN BEFORE ANY GOROUTINE EXISTS.
	//
	// CombinedOutput does Start and Wait together, so the only way to reach the
	// pid was to read cmd.Process from the containment path while that call was
	// still running — a data race, found by -race, in the code that runs
	// precisely when something has already gone wrong. Starting here means the
	// pid is owned before anything else can touch the command.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// Wait has returned, so the writers are closed and the buffer is ours.
		return buf.String(), err
	case <-time.After(bound):
		// The GROUP, not just the parent.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-done:
			return buf.String() + "\n[runner: containment timeout, process group killed]", errTimeout
		case <-time.After(10 * time.Second):
			// THE BUFFER IS NOT READ HERE. Something is still writing to it, and
			// reading it to produce a nicer message would be the same race this
			// function was rewritten to remove.
			return "[runner: containment timeout; the process group did not reap within the " +
				"bound, so no output is available]", errTimeout
		}
	}
}

var errTimeout = fmt.Errorf("mutate: containment timeout")

// firstFailure keeps the assertion and drops the rest: a verdict is read far
// more often than a full log.
func firstFailure(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--- FAIL") || strings.Contains(t, ".go:") {
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

// restore puts a mutated file back exactly as it was found, and proves it.
//
// os.WriteFile's PERMISSION ARGUMENT ONLY APPLIES WHEN IT CREATES THE FILE.
// Writing over an existing path leaves the old mode untouched, so the previous
// version claimed to restore the mode and did nothing of the kind — and its
// meta-cell could not tell, because it began and ended at 0644 and never
// perturbed the mode. A control that changes an executable's permission would
// have left it changed while the runner reported a clean restore, and the next
// identity digest would report a difference nobody could explain.
func restore(path string, body []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, body, mode); err != nil {
		return err
	}
	// EXPLICIT, because the write above will not do it for an existing file.
	if err := os.Chmod(path, mode.Perm()); err != nil {
		return err
	}
	after, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(after) != string(body) {
		return errNotRestored
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("mode is %v after restore, want %v", st.Mode().Perm(), mode.Perm())
	}
	return nil
}

var errNotRestored = fmt.Errorf("mutate: the file did not restore to its original bytes")
