package gatemutation

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
		// IN THE PACKAGE THE CONTROL NAMES, not merely somewhere in the tree.
		// A cell that exists in a different package is one the runner's
		// `-run` will never select, so the control would apply its break and
		// score a verdict against a test that never executed.
		if pkgs := names[m.Test]; len(pkgs) > 0 && !pkgs[m.Package] {
			where := make([]string, 0, len(pkgs))
			for p := range pkgs {
				where = append(where, p)
			}
			sort.Strings(where)
			t.Errorf("control %q names %s in package %s, but that test is declared in %v",
				m.Name, m.Test, m.Package, where)
		}
		if len(names[m.Test]) == 0 {
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
		// THE PACKAGE IS PART OF THE ADDRESS. A runner handed a bare test name
		// must guess which package to run, and a wrong guess runs nothing --
		// which is indistinguishable, in a ledger, from a control that ran and
		// failed to discriminate.
		if !strings.HasPrefix(m.Package, "./") || !strings.HasSuffix(m.Package, "/") {
			t.Errorf("control %q names package %q; want a go-test path like ./core/exec/",
				m.Name, m.Package)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, strings.TrimPrefix(m.Package, "./"))); err != nil {
			t.Errorf("control %q names package %q, which does not exist: %v", m.Name, m.Package, err)
		}
		if len(m.Guarantee) < 30 {
			t.Errorf("control %q does not say what goes unproven if it survives; a green "+
				"verdict would be unreadable", m.Name)
		}
		if strings.Contains(m.File, "..") || filepath.IsAbs(m.File) {
			t.Errorf("control %q names a path outside the repository: %s", m.Name, m.File)
		}
	}
	// THE COUNT IS EXACT, NOT A FLOOR. A floor of ten let thirteen controls
	// become ten without a word, which is how coverage is lost quietly --
	// three guarantees would stop being proven and every run would still look
	// complete. Adding a control means raising this number deliberately;
	// removing one means saying so out loud, here, in a diff somebody reads.
	const declared = 13
	if n := len(All()); n != declared {
		t.Errorf("there are %d controls and this cell pins %d. If a control was added, raise "+
			"the number. If one was removed, say which guarantee stopped being proven and "+
			"why that is acceptable", n, declared)
	}
}

// declaredTests maps every Test function name to the packages declaring it.
func declaredTests(t *testing.T) map[string]map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	names := map[string]map[string]bool{}
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
		pkg := "./" + filepath.ToSlash(filepath.Dir(strings.TrimPrefix(path, repoRoot+"/"))) + "/"
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			if names[m[1]] == nil {
				names[m[1]] = map[string]bool{}
			}
			names[m[1]][pkg] = true
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
