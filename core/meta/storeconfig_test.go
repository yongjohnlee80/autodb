package meta_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// config.Meta satisfies the interface this package declares.
//
// A COMPILE-TIME WITNESS, in the package that made the promise. The methods
// live in core/config, so a rename there breaks a build somewhere — and
// without this line, "somewhere" is whichever caller happens to pass one,
// which on a bad day is a test in a fourth package with an error message about
// an argument type rather than about a contract.
var _ meta.StoreConfig = config.Meta{}

// The store must not import the configuration layer.
//
// THE POINT OF THE INTERFACE, held as a fact rather than an intention. Before
// this change core/meta imported core/config in order to name the struct its
// own functions took, which is a storage layer depending on how a config file
// is parsed. The interface removed the edge; nothing stops a later import
// putting it back, and it would go back the easy way — someone needs one more
// field, reaches for config.Meta, and the compiler says yes.
//
// TESTS ARE EXEMPT, deliberately and by name. A test constructing a real
// config.Meta and handing it to meta.Open is the best available evidence that
// the structural satisfaction actually works at a call site, which is worth
// more than the purity of the test binary's import graph. The exemption is
// narrow: _test.go only, in this package only.
func TestTheStoreDoesNotImportTheConfigLayer(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	files := 0
	var offenders []string
	for _, pkg := range pkgs {
		for path, f := range pkg.Files {
			files++
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "github.com/yongjohnlee80/autodb/core/config" {
					offenders = append(offenders,
						path+":"+itoa(fset.Position(imp.Pos()).Line))
				}
			}
		}
	}
	// A walk that parsed nothing agrees with every claim about the package.
	if files < 10 {
		t.Fatalf("parsed only %d non-test file(s) in core/meta; a clean result "+
			"here would mean the walk is not reaching them", files)
	}
	if len(offenders) > 0 {
		t.Errorf("core/meta imports core/config at %s.\n\n"+
			"The store declares what it needs (StoreConfig) and the config layer "+
			"satisfies it structurally. Add the value to that interface instead — "+
			"if the store genuinely needs a fifth thing, it should say so in its "+
			"own words rather than borrow a struct that knows about TOML.",
			strings.Join(offenders, ", "))
	}
}

// Every StoreConfig method must be READ by this package.
//
// The inverse of the guard above, and the reason it exists: an interface the
// consumer declares can grow items the consumer does not use, at which point it
// is no longer "what the store needs" but a copy of the struct it replaced —
// which would put the coupling back in a shape no import guard can see.
func TestEveryStoreConfigMethodIsUsed(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	declared := map[string]bool{}
	used := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if ok && gd.Tok == token.TYPE {
					for _, spec := range gd.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || ts.Name.Name != "StoreConfig" {
							continue
						}
						it, ok := ts.Type.(*ast.InterfaceType)
						if !ok {
							continue
						}
						for _, m := range it.Methods.List {
							for _, n := range m.Names {
								declared[n.Name] = true
							}
						}
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					used[sel.Sel.Name] = true
				}
				return true
			})
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no StoreConfig methods; the walk missed the interface and " +
			"an unused method would now pass unnoticed")
	}
	for name := range declared {
		if !used[name] {
			t.Errorf("StoreConfig declares %s and nothing in core/meta calls it. "+
				"An interface the consumer does not consume is a copy of the "+
				"struct it replaced, wearing a different name.", name)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
