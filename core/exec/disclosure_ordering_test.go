package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The witness that lets the front door say "we are full" instead of
// "authentication failed" is EARNED BY POSITION, and this test is what keeps
// it that way.
//
// denyAfterAuthorization is only truthful where it is unreachable without a
// verified credential and a checked grant. Move one capacity check above
// either gate -- an entirely reasonable-looking optimisation, since refusing
// early is cheaper than authenticating first -- and that call now runs for an
// anonymous peer. The wire would start answering "the database is at its
// connection limit" to anyone with a TCP route, which is a capacity oracle:
// it reports live load to someone who has proved nothing.
//
// Nothing about the resulting code looks wrong at the call site. The defect
// is the ORDER, so the test reads the order.
func TestDisclosureWitness_IsUnreachableBeforeBothGates(t *testing.T) {
	const (
		authentication = "VerifyPAT"     // proves WHO the caller is
		authorization  = "AuthorizeUser" // proves they may hold this connection
	)
	// EVERY CONSTRUCTOR THAT STAMPS THE WITNESS, not just the first one.
	//
	// This was a single name, and a second constructor was added later that
	// sets the same authorized flag while carrying operator detail. The walk
	// could not see it, so calls to it were unguarded from the day it existed
	// -- the guard was silent about a path it was written to cover, which is
	// the failure mode it otherwise refuses to have. Any future constructor
	// that sets `authorized` belongs in this set.
	witnesses := map[string]bool{
		"denyAfterAuthorization":           true,
		"denyAfterAuthorizationWithDetail": true,
	}
	const witness = "denyAfterAuthorization*"

	type site struct {
		fn    string
		where token.Position
		pos   token.Pos
		gates map[string]token.Pos
	}

	var sites []site
	for _, path := range goSourceFiles(t) {
		// One FileSet per file keeps every position below comparable only
		// against positions from the same file, which is the only comparison
		// this test makes.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var calls []token.Pos
			gates := map[string]token.Pos{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if witnesses[fun.Name] {
						calls = append(calls, call.Pos())
					}
				case *ast.SelectorExpr:
					// FIRST occurrence wins. A later re-check cannot
					// retroactively authorize something that already ran.
					if name := fun.Sel.Name; name == authentication || name == authorization {
						if _, seen := gates[name]; !seen {
							gates[name] = call.Pos()
						}
					}
				}
				return true
			})
			for _, c := range calls {
				sites = append(sites, site{
					fn: fn.Name.Name, where: fset.Position(c), pos: c, gates: gates,
				})
			}
		}
	}

	// FAILS CLOSED. If the walk finds nothing it has stopped testing anything
	// -- a rename, a wrapper, a move to another package -- and silence would
	// read exactly like success.
	if len(sites) == 0 {
		t.Fatalf("no %s call sites found; the walk no longer sees the code it "+
			"guards, so its silence proves nothing", witness)
	}

	for _, s := range sites {
		for _, gate := range []string{authentication, authorization} {
			gpos, ok := s.gates[gate]
			if !ok {
				t.Errorf("%s: %s is called in %s, which never calls %s -- the "+
					"refusal claims the caller was authorized and nothing in "+
					"that function established it",
					s.where, witness, s.fn, gate)
				continue
			}
			if s.pos < gpos {
				t.Errorf("%s: %s runs BEFORE %s in %s. A caller who has not passed "+
					"that gate would be told the system is at its connection limit, "+
					"which reports live capacity to a peer who has proved nothing",
					s.where, witness, gate, s.fn)
			}
		}
	}
}
