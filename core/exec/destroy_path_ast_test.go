package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// THE FRAME TRICK MUST NOT COME BACK, AND A COMMENT SAYING SO IS NOT A GUARD.
//
// The teardown for a backend that could not be proved clean used to queue an
// unflushed ClosePortal so that golib's own reuse predicate would refuse the
// member. That coupled this product's isolation guarantee to an unexported
// predicate in a library: widen the predicate for a good reason and a failed
// reset silently becomes a reuse of contaminated state, with nothing to
// compile against. It was replaced by an explicit destruction the driver
// offers, and the capability is now required of every pin.
//
// WHAT MAKES THAT PERMANENT IS THIS CELL. The replacement is one small edit
// away from being undone by someone restoring a "fallback for older drivers",
// and that edit compiles, passes every behavioural test that counts teardowns,
// and reintroduces the coupling. So the source itself is walked: no function
// on the destruction path may mention ClosePortal at all.
//
// IT WALKS THE AST RATHER THAN GREPPING, because a grep for the string matches
// this file's own comments — which is exactly the shape of vacuous check that
// passed while proving nothing in an earlier round here. Identifiers in the
// parsed tree are identifiers.
func TestDestroyPath_QueuesNoFrameToSteerTheDriversReuseTest(t *testing.T) {
	// The functions a backend reaches on its way out of a session's hands.
	onThePath := map[string]bool{
		"destroyBackend":    true,
		"releaseBackend":    true,
		"proveBackendClean": true,
		"runResetPlan":      true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing core/exec: %v", err)
	}

	seen := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !onThePath[fn.Name.Name] {
					continue
				}
				seen++
				ast.Inspect(fn, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if ok && strings.Contains(id.Name, "ClosePortal") {
						t.Errorf("%s mentions %s at %s: the destruction path may not put a "+
							"handle into a state the driver's private reuse test happens to "+
							"reject. Destroy it, or say plainly that it cannot be destroyed",
							fn.Name.Name, id.Name, fset.Position(id.Pos()))
					}
					return true
				})
			}
		}
	}

	// THE VACUITY FLOOR. A walk that found no functions would pass silently,
	// and a rename is exactly how this check would stop watching anything.
	if seen != len(onThePath) {
		t.Fatalf("walked %d of the %d functions on the destruction path; the names in this "+
			"cell no longer match the code and it is guarding nothing", seen, len(onThePath))
	}
}
