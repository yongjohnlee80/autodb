package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The core carries admission Reason.Code but must never inspect it. This source
// walk makes a restored per-code switch, comparison, or lookup fail even when
// all currently registered codes happen to retain mappings.
func TestCoreExecDoesNotInspectAdmissionReasonCode(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Code" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "admission" {
				return true
			}
			pos := fset.Position(sel.Pos())
			t.Errorf("%s:%d inspects .Code in core/exec; carry admission Reason.Code without interpreting it", pos.Filename, pos.Line)
			return true
		})
	}
}

// A22 keeps wire vocabulary out of the analyzer currency. The named mutation
// is adding SQLState (or another protocol-shaped field) to admission.Reason;
// this AST walk must fail before a surface can start depending on it.
func TestAdmissionReasonHasNoProtocolVocabulary(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../admission/stage.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"sqlstate", "pgproto", "postgres", "protocol", "wire"}
	foundReason := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Reason" {
				continue
			}
			foundReason = true
			strct, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("admission.Reason is no longer a struct")
			}
			for _, field := range strct.Fields.List {
				for _, name := range field.Names {
					lower := strings.ToLower(name.Name)
					for _, word := range forbidden {
						if strings.Contains(lower, word) {
							t.Errorf("admission.Reason field %q carries protocol vocabulary %q", name.Name, word)
						}
					}
				}
			}
		}
	}
	if !foundReason {
		t.Fatal("admission.Reason declaration not found")
	}
}
