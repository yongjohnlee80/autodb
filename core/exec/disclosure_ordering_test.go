package exec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
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
	for _, v := range disclosureViolations(t, packageSources(t)) {
		t.Error(v)
	}
}

// THE GUARD TELLS A SAFE COMPOSITION FROM A BYPASS. OpenWireSessionWith is
// respelled two ways in the real source: admitWire called on the authorization
// AuthorizeWire just returned is the same flow and must pass; admitWire called on
// an authorization nobody issued reaches a capacity answer with no credential
// or grant checked and must be caught. A guard that flagged both would be
// enforcing a spelling, and one that flagged neither would be guarding nothing.
func TestDisclosureWitness_TellsASafeCompositionFromABypass(t *testing.T) {
	const composed = "\ta, err := e.AuthorizeWire(ctx, req)\n\tif err != nil {\n\t\treturn WireSessionResult{}, err\n\t}\n\treturn a.Admit(ctx)\n}\n"
	stock := packageSources(t)
	if n := strings.Count(string(stock["wire_session.go"]), composed); n != 1 {
		t.Fatalf("OpenWireSessionWith's composition appears %d times in wire_session.go, want 1; "+
			"this cell's anchor has moved", n)
	}
	for _, c := range []struct {
		name, with string
		safe       bool
	}{
		{"admitWire on the authorization AuthorizeWire returned",
			"\ta, err := e.AuthorizeWire(ctx, req)\n\tif err != nil {\n\t\treturn WireSessionResult{}, err\n\t}\n\treturn e.admitWire(ctx, a)\n}\n", true},
		{"admitWire on an authorization nobody issued",
			"\treturn e.admitWire(ctx, &WireAuthorization{req: req})\n}\n", false},
	} {
		srcs := map[string][]byte{}
		for k, v := range stock {
			srcs[k] = v
		}
		srcs["wire_session.go"] = []byte(strings.Replace(string(stock["wire_session.go"]), composed, c.with, 1))
		v := disclosureViolations(t, srcs)
		if c.safe && len(v) > 0 {
			t.Errorf("%s: the guard flagged an equivalent composition: %v", c.name, v)
		}
		if !c.safe && len(v) == 0 {
			t.Errorf("%s: the guard did not catch a capacity answer reached with no credential or grant checked", c.name)
		}
	}
}

// packageSources reads the package's non-test sources, keyed by file name.
func packageSources(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, path := range goSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = b
	}
	return out
}

// disclosureViolations is the guard over a set of sources, returning one message
// per capacity witness that a caller who has not passed both gates could reach.
func disclosureViolations(t *testing.T, sources map[string][]byte) []string {
	t.Helper()
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

	// A FUNCTION THAT DOES NOT CHECK BOTH GATES ITSELF MAY STILL BE PAST THEM:
	// a login that waits for its pool is authorized in one function and admitted
	// in others. Such a function is trusted only if it is unexported, has at
	// least one caller, and EVERY reference to it -- a call or a method value --
	// sits after both gates in its caller, or in a caller trusted the same way.
	// An exported function is never trusted this way: a caller outside the
	// package is one this walk cannot see.
	//
	// A GATE MAY BE PASSED THROUGH A HELPER. A call to a function of this
	// package that itself passes a gate -- directly or through another such
	// function, AuthorizeWire included -- counts as passing that gate at the
	// call. Exported or not makes no difference here: what matters is what the
	// callee does, not who else may call it.
	type call struct {
		callee string
		pos    token.Pos
	}
	type fnInfo struct {
		calls []call
		count int // declarations sharing this name
	}
	type site struct {
		fn    string
		where token.Position
		pos   token.Pos
	}
	type ref struct {
		caller string
		pos    token.Pos
	}

	fns := map[string]*fnInfo{}
	refsTo := map[string][]ref{}
	var sites []site
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		// One FileSet per file keeps every position below comparable only
		// against positions from the same file, which is the only comparison
		// this test makes; a caller's ref and its gates come from one file.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, sources[path], 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			info := fns[name]
			if info == nil {
				info = &fnInfo{}
				fns[name] = info
			}
			info.count++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fun := x.Fun.(type) {
					case *ast.Ident:
						if witnesses[fun.Name] {
							sites = append(sites, site{fn: name, where: fset.Position(x.Pos()), pos: x.Pos()})
						}
						info.calls = append(info.calls, call{callee: fun.Name, pos: x.Pos()})
					case *ast.SelectorExpr:
						info.calls = append(info.calls, call{callee: fun.Sel.Name, pos: x.Pos()})
					}
				case *ast.SelectorExpr:
					refsTo[x.Sel.Name] = append(refsTo[x.Sel.Name], ref{caller: name, pos: x.Pos()})
				case *ast.Ident:
					refsTo[x.Name] = append(refsTo[x.Name], ref{caller: name, pos: x.Pos()})
				}
				return true
			})
		}
	}

	// FAILS CLOSED. If the walk finds nothing it has stopped testing anything
	// -- a rename, a wrapper, a move to another package -- and silence would
	// read exactly like success.
	if len(sites) == 0 {
		return []string{fmt.Sprintf("no %s call sites found; the walk no longer sees the code it "+
			"guards, so its silence proves nothing", witness)}
	}

	// passing[gate] is every function that passes gate somewhere in its body,
	// directly or through a function of this package that does: a fixed point,
	// grown until no call adds a function.
	passing := map[string]map[string]bool{}
	for _, gate := range []string{authentication, authorization} {
		set := map[string]bool{}
		for grew := true; grew; {
			grew = false
			for name, info := range fns {
				if set[name] || info.count != 1 {
					continue
				}
				for _, c := range info.calls {
					if c.callee == gate || set[c.callee] {
						set[name], grew = true, true
						break
					}
				}
			}
		}
		passing[gate] = set
	}
	// gatedAt reports whether pos, inside fn, runs after both gates. The FIRST
	// call that passes a gate is where fn passes it: a later re-check cannot
	// retroactively authorize something that already ran.
	gatedAt := func(fn string, pos token.Pos) (missing string, before bool) {
		for _, gate := range []string{authentication, authorization} {
			gpos := token.NoPos
			for _, c := range fns[fn].calls {
				if (gpos == token.NoPos || c.pos < gpos) &&
					(c.callee == gate || passing[gate][c.callee]) {
					gpos = c.pos
				}
			}
			if gpos == token.NoPos {
				return gate, false
			}
			if pos < gpos {
				return gate, true
			}
		}
		return "", false
	}
	var trusted func(fn string, seen map[string]bool) bool
	trusted = func(fn string, seen map[string]bool) bool {
		info := fns[fn]
		if info == nil || info.count != 1 || ast.IsExported(fn) || seen[fn] {
			return false // a cycle proves nothing: a root must check the gates
		}
		seen[fn] = true
		defer delete(seen, fn)
		callers := refsTo[fn]
		if len(callers) == 0 {
			return false
		}
		for _, r := range callers {
			if missing, _ := gatedAt(r.caller, r.pos); missing == "" {
				continue
			}
			if !trusted(r.caller, seen) {
				return false
			}
		}
		return true
	}

	var out []string
	for _, s := range sites {
		missing, before := gatedAt(s.fn, s.pos)
		switch {
		case missing == "":
		case before:
			out = append(out, fmt.Sprintf("%s: %s runs BEFORE %s in %s. A caller who has not passed "+
				"that gate would be told the system is at its connection limit, "+
				"which reports live capacity to a peer who has proved nothing",
				s.where, witness, missing, s.fn))
		case !trusted(s.fn, map[string]bool{}):
			out = append(out, fmt.Sprintf("%s: %s is called in %s, which never calls %s, and not every "+
				"path into %s passes it -- the refusal claims the caller was "+
				"authorized and nothing on some path to it established it",
				s.where, witness, s.fn, missing, s.fn))
		}
	}
	return out
}
