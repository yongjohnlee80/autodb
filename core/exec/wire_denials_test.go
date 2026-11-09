package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// EACH HALF OF A FRONT-DOOR LOGIN DECLARES EXACTLY WHAT IT RAISES.
//
// The front door's credential phase calls AuthorizeWire and its admission phase
// calls Admit (admitWire), and each phase declares the engine's denials it can
// end on from AuthorizeDenials and AdmitDenials. A list that is too broad lets a
// phase end on an outcome it cannot reach, so a wiring mistake is accepted
// rather than refused; one that is too narrow refuses a real denial as a
// programming error, during an incident.
//
// So the sets are derived from the source and compared both ways. The walk
// starts at each half's entry point and follows calls to functions declared in
// wire_session.go, collecting every Deny reason a reached function names. A
// function literal that is stored rather than called runs later, under
// whoever calls it, so its body belongs to that caller's half: AuthorizeWire's
// admitter closure is admission's, not authorization's.
//
// THE WALK IS COMPLETE ONLY BECAUSE NO OTHER FILE MAKES A WIRE DENIAL, and
// that is asserted too: a denial constructed or a Deny reason named anywhere
// else in the package would be one this walk cannot see.
func TestWireDenials_EachHalfDeclaresExactlyWhatItRaises(t *testing.T) {
	const home = "wire_session.go"
	constructors := map[string]bool{
		"deny": true, "denyAfterAuthorization": true, "denyAfterAuthorizationWithDetail": true, "WireDenial": true,
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, home, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(n.Name, "Deny") {
					reasons[n.Name] = true
				}
			}
		}
	}
	if len(reasons) < len(DenialReasons()) {
		t.Fatalf("found %d Deny constants in %s, but DenialReasons lists %d; the walk no longer sees "+
			"where they are declared", len(reasons), home, len(DenialReasons()))
	}

	type fnFacts struct {
		denies  map[string]bool
		callees map[string]bool
	}
	funcs := map[string][]fnFacts{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		facts := fnFacts{denies: map[string]bool{}, callees: map[string]bool{}}
		called := map[*ast.FuncLit]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if lit, ok := c.Fun.(*ast.FuncLit); ok {
					called[lit] = true
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return called[x] // a stored closure runs under its caller
			case *ast.CallExpr:
				switch fun := x.Fun.(type) {
				case *ast.Ident:
					facts.callees[fun.Name] = true
				case *ast.SelectorExpr:
					facts.callees[fun.Sel.Name] = true
				}
			case *ast.Ident:
				if reasons[x.Name] {
					facts.denies[x.Name] = true
				}
			}
			return true
		})
		funcs[fd.Name.Name] = append(funcs[fd.Name.Name], facts)
	}

	raised := func(root string) []string {
		if len(funcs[root]) == 0 {
			t.Fatalf("%s is not declared in %s; the walk has lost its entry point", root, home)
		}
		seen, out := map[string]bool{}, map[string]bool{}
		var walk func(name string)
		walk = func(name string) {
			if seen[name] {
				return
			}
			seen[name] = true
			for _, facts := range funcs[name] {
				for r := range facts.denies {
					out[r] = true
				}
				for c := range facts.callees {
					walk(c)
				}
			}
		}
		walk(root)
		return sortedKeys(out)
	}
	asNames := func(values []string) []string {
		byValue := map[string]string{}
		for name := range reasons {
			byValue[constValue(t, f, name)] = name
		}
		var out []string
		for _, v := range values {
			out = append(out, byValue[v])
		}
		sort.Strings(out)
		return out
	}

	for _, half := range []struct {
		root     string
		declared []string
		list     string
	}{
		{"AuthorizeWire", AuthorizeDenials(), "AuthorizeDenials"},
		{"admitWire", AdmitDenials(), "AdmitDenials"},
	} {
		got, want := raised(half.root), asNames(half.declared)
		if len(got) == 0 {
			t.Fatalf("the walk from %s found no denials; it has stopped seeing the code", half.root)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s declares %v, but the code reachable from %s raises %v",
				half.list, want, half.root, got)
		}
	}

	// No other file makes or names a wire denial.
	for _, path := range goSourceFiles(t) {
		if path == home {
			continue
		}
		other, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(other, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && constructors[id.Name] {
					t.Errorf("%s calls %s: a wire denial made outside %s is one the per-half walk cannot see",
						path, id.Name, home)
				}
			case *ast.Ident:
				if reasons[x.Name] {
					t.Errorf("%s names %s: a wire denial reason used outside %s is one the per-half walk "+
						"cannot see", path, x.Name, home)
				}
			}
			return true
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// constValue reads a string constant's literal value from the parsed file.
func constValue(t *testing.T, f *ast.File, name string) string {
	t.Helper()
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return strings.Trim(lit.Value, "`\"")
				}
			}
		}
	}
	t.Fatalf("constant %s has no string literal value in the file", name)
	return ""
}
