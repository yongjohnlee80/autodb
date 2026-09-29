// Package notesguard holds the note paths of tui to the rule that they do
// no filesystem work of their own: the one owner of the notes filesystem
// contract (confinement, no symlinks, modes, atomic writes, conflict
// detection) is core/notes, and a second implementation in tui would drift
// from it.
package notesguard_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// forbidden are the imports that would let a tui note file touch the
// filesystem itself.
var forbidden = map[string]bool{"os": true, "syscall": true, "io/fs": true, "path/filepath": true}

// fsImports returns the forbidden imports of the Go source src.
func fsImports(t *testing.T, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if forbidden[p] {
			out = append(out, p)
		}
	}
	return out
}

// No non-test note file in tui imports os, syscall, io/fs or path/filepath.
func TestTUINotePathsDoNoFilesystemWork(t *testing.T) {
	files, err := filepath.Glob("../../tui/note*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if matched, _ := filepath.Match("*_test.go", filepath.Base(f)); matched {
			continue
		}
		checked++
		if bad := fsImports(t, f, nil); len(bad) > 0 {
			t.Errorf("%s imports %v: note filesystem work belongs in core/notes", f, bad)
		}
	}
	if checked == 0 {
		t.Fatal("no tui note files found: the guard checks nothing")
	}
}

// The check itself: a file importing os is caught, one that does not is not.
func TestTheGuardCatchesAnOsImport(t *testing.T) {
	if bad := fsImports(t, "bad.go", "package tui\nimport \"os\"\nvar _ = os.Remove\n"); len(bad) != 1 {
		t.Fatalf("an os import was not caught: %v", bad)
	}
	if bad := fsImports(t, "ok.go", "package tui\nimport \"strings\"\nvar _ = strings.Cut\n"); len(bad) != 0 {
		t.Fatalf("a clean file was flagged: %v", bad)
	}
}
