package exec

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The admission leaf's import boundary, asserted rather than trusted.
//
// core/admission is the seam the future analyzer implements. Everything
// the layering promises rests on what that package CANNOT reach: no
// protocol library (the analyzer must stay reusable by surfaces with no
// wire vocabulary), no frontdoor (the renderer owns the surface's
// vocabulary, not the analyzer), and no core/exec — the leaf's types are
// accessor-shaped precisely so the engine implements the interfaces from
// OUTSIDE, and an import in this direction would be a cycle the moment the
// drives compose the orchestrator.
//
// The assertion is structural: parse the leaf's non-test sources and read
// the import sets. A violation fails the build here, not in review.
func TestAdmissionLeaf_ImportsNothingFromEngineOrWire(t *testing.T) {
	const dir = "../admission"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("core/admission is missing (%v) — the seam cannot be asserted", err)
	}
	filesChecked := 0
	forbidden := map[string]string{
		"github.com/yongjohnlee80/autodb/core/exec": "the leaf must not import the engine — the drives compose the orchestrator, so this direction would be an import cycle",
		"github.com/yongjohnlee80/autodb/frontdoor": "the analyzer must not know the front door; the SURFACE renders a Reason into its own vocabulary",
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		filesChecked++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for bad, why := range forbidden {
				if path == bad {
					t.Errorf("%s imports %q — %s", name, bad, why)
				}
			}
			// The wire protocol libraries, by prefix: pgproto3, pgconn,
			// golib's wire-facing packages. The analyzer that names a
			// SQLSTATE or a backend message is an analyzer the RPC
			// surface cannot reuse.
			if strings.Contains(path, "pgproto3") || strings.Contains(path, "pgconn") {
				t.Errorf("%s imports %q — a protocol library in the leaf makes the "+
					"analyzer a wire component", name, path)
			}
		}
	}
	if filesChecked == 0 {
		t.Fatal("no non-test sources found in core/admission — this cell asserted nothing")
	}
}
