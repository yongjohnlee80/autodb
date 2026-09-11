package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestAdmissionRegistry_IncludesEveryProductionAdapter(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	discovered := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "DenyCodes" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			typ := fn.Recv.List[0].Type
			if ptr, ok := typ.(*ast.StarExpr); ok {
				typ = ptr.X
			}
			if ident, ok := typ.(*ast.Ident); ok {
				discovered[ident.Name] = true
			}
		}
	}

	registered := map[string]bool{}
	for _, stage := range registeredAdmissionStages {
		typ := reflect.TypeOf(stage)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		registered[typ.Name()] = true
	}

	var missing, stale []string
	for typ := range discovered {
		if !registered[typ] {
			missing = append(missing, typ)
		}
	}
	for typ := range registered {
		if !discovered[typ] {
			stale = append(stale, typ)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) != 0 || len(stale) != 0 {
		t.Fatalf("admission registry differs from production adapters: missing=%v stale=%v", missing, stale)
	}
}
