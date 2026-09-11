package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestConnectionManager_UsesExposureNotCapabilityProfile(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "managers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	findFunc := func(name string) *ast.FuncDecl {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
				return fn
			}
		}
		return nil
	}
	manager := findFunc("openConnManager")
	switcher := findFunc("openExposureSwitch")
	if manager == nil || switcher == nil {
		t.Fatalf("connection manager functions missing: manager=%v switcher=%v", manager != nil, switcher != nil)
	}
	managerReadsExposure := false
	ast.Inspect(manager.Body, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "FrontDoorExposed" {
			managerReadsExposure = true
		}
		return true
	})
	var switchReadsExposure, switchSetsExposure bool
	ast.Inspect(switcher.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Profile", "SetConnectionProfile":
			t.Errorf("front-door switch still uses capability concern %q", sel.Sel.Name)
		case "FrontDoorExposed":
			switchReadsExposure = true
		case "SetConnectionExposure":
			switchSetsExposure = true
		}
		return true
	})
	if !managerReadsExposure || !switchReadsExposure || !switchSetsExposure {
		t.Fatalf("connection manager exposure plumbing incomplete: table=%v switch=%v setter=%v",
			managerReadsExposure, switchReadsExposure, switchSetsExposure)
	}
}
