package gatemutation

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is two levels up from this package.
const repoRoot = "../.."

// EVERY CONTROL STILL POINTS AT THE CODE IT MEANS TO BREAK.
//
// THIS IS THE CELL THE PROCESS WAS MISSING. A control whose anchor no longer
// matches is applied to nothing, and a run can still report a verdict for it —
// so a guarantee stops being proven and the ledger says otherwise. Exactly once
// is the property: twice would mutate something the control never intended, and
// zero times would mutate nothing at all.
func TestMutations_EveryAnchorMatchesExactlyOnce(t *testing.T) {
	for _, m := range All() {
		t.Run(m.Name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(repoRoot, m.File))
			if err != nil {
				t.Fatalf("%s: %v", m.File, err)
			}
			switch n := strings.Count(string(body), m.Anchor); n {
			case 1: // the only acceptable answer
			case 0:
				t.Errorf("the anchor is gone from %s, so this control now breaks NOTHING and "+
					"a run would still score it. Unproven if it stays this way: %s",
					m.File, m.Guarantee)
			default:
				t.Errorf("the anchor matches %d times in %s, so applying it would change code "+
					"this control never meant to touch", n, m.File)
			}
		})
	}
}

// EVERY CONTROL NAMES A TEST THAT EXISTS.
//
// THIS CELL EXISTS BECAUSE THE OPPOSITE HAPPENED. A control named a cell that
// had been deleted by an editing mistake, so nothing could have caught the
// break it applied; the run scored it INVALID only because the runner thought
// to check, and no part of the process would otherwise have noticed that a
// guarantee had silently stopped being proven.
func TestMutations_EveryNamedTestExists(t *testing.T) {
	names := declaredTests(t)
	for _, m := range All() {
		if !names[m.Test] {
			t.Errorf("control %q names %s, which no test file declares. Nothing can catch the "+
				"break it applies, and what goes unproven is: %s", m.Name, m.Test, m.Guarantee)
		}
	}
}

// EVERY CONTROL IS DISTINCT AND SAYS WHAT IT PROTECTS.
//
// A duplicate name makes two ledgers incomparable; a missing guarantee makes a
// green verdict unreadable, because the reviewer cannot tell what survived.
func TestMutations_TheSetIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range All() {
		if seen[m.Name] {
			t.Errorf("two controls are both named %q, so a ledger cannot say which survived", m.Name)
		}
		seen[m.Name] = true

		if m.Anchor == "" || m.Replacement == m.Anchor {
			t.Errorf("control %q changes nothing", m.Name)
		}
		if !strings.HasPrefix(m.Test, "Test") {
			t.Errorf("control %q names %q, which is not a test", m.Name, m.Test)
		}
		if len(m.Guarantee) < 30 {
			t.Errorf("control %q does not say what goes unproven if it survives; a green "+
				"verdict would be unreadable", m.Name)
		}
		if strings.Contains(m.File, "..") || filepath.IsAbs(m.File) {
			t.Errorf("control %q names a path outside the repository: %s", m.Name, m.File)
		}
	}
	if len(All()) < 10 {
		t.Errorf("only %d controls: the set has shrunk, which is how coverage is lost quietly",
			len(All()))
	}
}

// declaredTests collects every Test function name in the repository.
func declaredTests(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	names := map[string]bool{}
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			names[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no test functions found at all; the walk is broken, not the controls")
	}
	return names
}
