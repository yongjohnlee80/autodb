package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A1-C1: the raw simple-query capability is reachable ONLY through the engine's
// gate. Structurally: no package outside core/exec references golib's pinned
// connection or its simple-query face, and core/exec holds exactly ONE type
// assertion to SimpleQuerier — the dispatch site that follows the gate. A
// frontdoor reference to the capability is the substitution mutation that must
// redden this cell.
func TestRawSimpleQueryCapabilityNeverLeavesCoreExec(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	// The capability's names are forbidden everywhere outside core/exec. The
	// driver package itself is forbidden on the four surfaces A1-C1 names — the
	// meta store legitimately imports it for its own schema and is not one.
	forbidden := []string{"SimpleQuerier", "SimpleQuery(", "PinnedConn", "PinSessionConn"}
	noDriverImport := []string{"frontdoor", "tui", "webserver", "rpc"}

	var outside []string
	err := filepath.WalkDir(repo, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(repo, path)
		if strings.HasPrefix(rel, filepath.Join("core", "exec")+string(filepath.Separator)) {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				outside = append(outside, rel+": "+f)
			}
		}
		top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		for _, surface := range noDriverImport {
			if top == surface && strings.Contains(string(src), "golib/dao/postgres") {
				outside = append(outside, rel+": imports golib/dao/postgres")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) > 0 {
		t.Fatalf("the raw pinned capability is referenced outside core/exec:\n  %s", strings.Join(outside, "\n  "))
	}

	// Exactly one SimpleQuerier type assertion in core/exec production code.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(repo, "core", "exec"), func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				ta, ok := n.(*ast.TypeAssertExpr)
				if !ok || ta.Type == nil {
					return true
				}
				if sel, ok := ta.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "SimpleQuerier" {
					sites = append(sites, filepath.Base(name)+":"+fset.Position(ta.Pos()).String())
				}
				return true
			})
		}
	}
	if len(sites) != 1 {
		t.Fatalf("SimpleQuerier is asserted at %d site(s) in core/exec, want exactly 1 (the gated dispatch): %v", len(sites), sites)
	}
}
