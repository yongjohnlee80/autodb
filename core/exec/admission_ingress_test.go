package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type ingressFunc struct {
	key, name, recv, file string
	decl                  *ast.FuncDecl
	calls                 map[string]bool
	classifies, executes  bool
	classifyAt, executeAt token.Pos
	sizeAt, policyAt      token.Pos
	classAt               token.Pos
}

func loadIngressFuncs(t *testing.T) (map[string]*ingressFunc, map[string][]string) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ingressFunc{}
	byName := map[string][]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			recv := receiverName(fn)
			key := fn.Name.Name
			if recv != "" {
				key = recv + "." + key
			}
			n := &ingressFunc{key: key, name: fn.Name.Name, recv: recv, file: path, decl: fn, calls: map[string]bool{}}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ingressCallName(call)
				if name != "" {
					n.calls[name] = true
				}
				if name == "Classify" {
					n.classifies = true
					n.classifyAt = call.Pos()
				}
				if name == "runSizeAdmission" {
					n.sizeAt = call.Pos()
				}
				if name == "runPrePolicyAdmission" || name == "runSessionAdmission" {
					n.policyAt = call.Pos()
				}
				if name == "runClassAdmission" {
					n.classAt = call.Pos()
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExecuteOp" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "golibpg" {
						n.executes = true
						n.executeAt = call.Pos()
					}
				}
				return true
			})
			if _, exists := funcs[key]; exists {
				t.Fatalf("duplicate production function key %q; ingress call graph needs receiver-qualified uniqueness", key)
			}
			funcs[key] = n
			byName[n.name] = append(byName[n.name], key)
		}
	}
	return funcs, byName
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func ingressCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	default:
		return ""
	}
}

func ingressRunAnchor(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		run, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || run.Sel.Name != "Run" {
			return true
		}
		composeCall, ok := run.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		compose, ok := composeCall.Fun.(*ast.SelectorExpr)
		if !ok || compose.Sel.Name != "Compose" {
			return true
		}
		pkg, ok := compose.X.(*ast.Ident)
		found = ok && pkg.Name == "admission"
		return !found
	})
	return found
}

func reachesIngress(start, anchor string, funcs map[string]*ingressFunc, byName map[string][]string) bool {
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if key == anchor {
			return true
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		for name := range funcs[key].calls {
			queue = append(queue, byName[name]...)
		}
	}
	return false
}

// A16 is discovered from executable shape, not maintained as a drive list:
// Engine methods that classify SQL, plus the method that sends ExecuteOp for a
// stored immutable classification, must all reach the sole orchestrator anchor.
func TestAdmissionIngressClosure_AllDiscoveredDrivesReachOrchestrator(t *testing.T) {
	funcs, byName := loadIngressFuncs(t)
	var anchors []string
	for key, fn := range funcs {
		if ingressRunAnchor(fn.decl) {
			anchors = append(anchors, key)
		}
	}
	if len(anchors) != 1 {
		t.Fatalf("found %d production admission.Compose(...).Run anchors %v, want exactly one", len(anchors), anchors)
	}

	var drives []string
	for key, fn := range funcs {
		if fn.recv == "Engine" && (fn.classifies || fn.executes) {
			drives = append(drives, key)
		}
	}
	sort.Strings(drives)
	for _, want := range []string{"Engine.WireExecutePortal", "Engine.WireParse", "Engine.executeSessionUnit", "Engine.gateWireStatement", "Engine.run"} {
		if !containsIngressKey(drives, want) {
			t.Fatalf("drive discovery missed %s; discovered %v", want, drives)
		}
	}
	for _, drive := range drives {
		fn := funcs[drive]
		if !reachesIngress(drive, anchors[0], funcs, byName) {
			t.Errorf("executable drive %s (%s) cannot reach the orchestrator", drive, fn.file)
		}
		if fn.classifies {
			if fn.sizeAt == token.NoPos || fn.sizeAt > fn.classifyAt {
				t.Errorf("classifying drive %s (%s) does not run size admission before Classify", drive, fn.file)
			}
			if fn.policyAt == token.NoPos || fn.policyAt < fn.classifyAt {
				t.Errorf("classifying drive %s (%s) does not run statement-policy admission after Classify", drive, fn.file)
			}
		}
		if fn.executes && (fn.classAt == token.NoPos || fn.classAt > fn.executeAt) {
			t.Errorf("execute drive %s (%s) does not run fresh class admission before ExecuteOp", drive, fn.file)
		}
	}
}

func containsIngressKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// A6 is the negative half: phase-1 intake and SQL-policy guards may be called
// only through their exact Stage adapter. Classification, authority resolution
// and special control floors, enforcement, wraps, routing, records and dispatch
// remain drive-owned by design.
func TestAdmissionIngressClosure_NoLegacyPolicyGuardCallsOutsideAdapters(t *testing.T) {
	funcs, _ := loadIngressFuncs(t)
	wantCalls := map[string]int{
		"Profile.admit":  1,
		"guardWhere":     1,
		"admitLock":      1,
		"admitSet":       1,
		"admitWireSet":   1,
		"admitWireReset": 1,
		"authorizeUnit":  1,
		"readerAnalysis": 1,
	}
	gotCalls := map[string]int{}
	sizeStageInstances := 0
	for _, fn := range funcs {
		ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
			if stmt, ok := node.(*ast.IfStmt); ok && containsSelector(stmt.Cond, "maxStatementBytes") {
				t.Errorf("raw statement-size guard survives outside sizeCapStage in %s (%s)", fn.key, fn.file)
			}
			if lit, ok := node.(*ast.CompositeLit); ok {
				if typ, ok := lit.Type.(*ast.Ident); ok && typ.Name == "sizeCapStage" {
					sizeStageInstances++
					if fn.key != "Engine.runSizeAdmission" {
						t.Errorf("sizeCapStage composed outside the pre-classification intake helper in %s (%s)", fn.key, fn.file)
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			guard := legacyPolicyCall(call)
			if guard == "" {
				return true
			}
			gotCalls[guard]++
			if !guardCallAllowed(fn, guard) {
				t.Errorf("legacy SQL-policy guard %s called outside its Stage adapter in %s (%s)", guard, fn.key, fn.file)
			}
			return true
		})
	}
	if sizeStageInstances != 1 {
		t.Errorf("production sizeCapStage instances = %d, want exactly one pre-classification adapter anchor", sizeStageInstances)
	}
	for guard, want := range wantCalls {
		if got := gotCalls[guard]; got != want {
			t.Errorf("production calls to %s = %d, want %d; adapter anchor moved or a bypass survived", guard, got, want)
		}
	}
}

func containsSelector(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func legacyPolicyCall(call *ast.CallExpr) string {
	name := ingressCallName(call)
	switch name {
	case "guardWhere", "admitLock", "admitSet", "admitWireSet", "admitWireReset", "authorizeUnit", "readerAnalysis":
		return name
	case "admit":
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "sessions" {
			return ""
		}
		return "Profile.admit"
	default:
		return ""
	}
}

func guardCallAllowed(fn *ingressFunc, guard string) bool {
	switch guard {
	case "Profile.admit":
		return fn.recv == "profileAdmitStage" && fn.name == "Apply"
	case "guardWhere":
		return fn.recv == "guardWhereStage" && fn.name == "Apply"
	case "admitLock", "admitSet", "admitWireSet", "admitWireReset":
		return fn.recv == "sessionStateStage" && fn.name == "Apply"
	case "authorizeUnit":
		return fn.recv == "authorizeUnitStage" && fn.name == "Apply"
	case "readerAnalysis":
		return fn.recv == "readerAnalysisStage" && fn.name == "Apply"
	default:
		return false
	}
}
