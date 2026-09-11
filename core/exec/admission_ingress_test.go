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
	targets               map[string]bool
	classifies, executes  bool
	dispatches            bool
	classifyAt, executeAt token.Pos
	dispatchAt            []token.Pos
	sizeAt, policyAt      token.Pos
	classAt, grammarAt    token.Pos
}

var ingressNonStatementDispatchExempt = map[string]string{
	"Engine.ListColumns":            "engine-authored catalog introspection, not client SQL",
	"Engine.ListRoutines":           "engine-authored catalog introspection, not client SQL",
	"Engine.ListSchemas":            "engine-authored catalog introspection, not client SQL",
	"Engine.ListTables":             "engine-authored catalog introspection, not client SQL",
	"Engine.ReconcileConnection":    "internal transaction-outcome recovery",
	"Engine.ReconcileOutcomes":      "internal transaction-outcome recovery",
	"Engine.StartOutcomeReconciler": "internal transaction-outcome recovery loop",
	"Engine.TestConnection":         "engine-authored target health probe",
	"Engine.WireBind":               "protocol object operation; executes no statement",
	"Engine.WireClosePortal":        "protocol object operation; executes no statement",
	"Engine.WireCloseStatement":     "protocol object operation; executes no statement",
	"Engine.WireDescribePortal":     "protocol object operation; executes no statement",
	"Engine.WireDescribeStatement":  "protocol object operation; executes no statement",
	"Engine.WireFlushSegment":       "protocol flush; executes no statement",
	"Engine.WireSyncSegment":        "protocol synchronization; executes no statement",
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
			n := &ingressFunc{
				key: key, name: fn.Name.Name, recv: recv, file: path, decl: fn,
				calls: map[string]bool{}, targets: map[string]bool{},
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ingressCallName(call)
				if name != "" {
					n.calls[name] = true
					n.targets[ingressCallTarget(call, fn)] = true
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
				if name == "runWireGrammarAdmission" {
					n.grammarAt = call.Pos()
				}
				if name == "queryOn" || name == "execOn" || name == "runQuery" || name == "runExec" || name == "sessionSimpleQuery" ||
					name == "QueryContext" || name == "ExecContext" || name == "SimpleQuery" {
					n.dispatches = true
					n.dispatchAt = append(n.dispatchAt, call.Pos())
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExecuteOp" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "golibpg" {
						n.executes = true
						n.dispatches = true
						n.executeAt = call.Pos()
						n.dispatchAt = append(n.dispatchAt, call.Pos())
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

func reachesTargetDispatch(start string, funcs map[string]*ingressFunc, byName map[string][]string) bool {
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		fn := funcs[key]
		if fn == nil {
			continue
		}
		if fn.dispatches {
			return true
		}
		for target := range fn.targets {
			queue = appendIngressTarget(queue, target, funcs, byName)
		}
	}
	return false
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

func ingressCallTarget(call *ast.CallExpr, fn *ast.FuncDecl) string {
	name := ingressCallName(call)
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return name
	}
	receiver, ok := sel.X.(*ast.Ident)
	if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 ||
		receiver.Name != fn.Recv.List[0].Names[0].Name {
		return name
	}
	return receiverName(fn) + "." + name
}

func appendIngressTarget(queue []string, target string, funcs map[string]*ingressFunc, byName map[string][]string) []string {
	if _, ok := funcs[target]; ok {
		return append(queue, target)
	}
	return append(queue, byName[target]...)
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
		fn := funcs[key]
		if fn == nil {
			continue
		}
		for target := range fn.targets {
			queue = appendIngressTarget(queue, target, funcs, byName)
		}
	}
	return false
}

// A16 is discovered from executable shape, not maintained as a drive list:
// Engine methods that classify SQL, plus the method that sends ExecuteOp for a
// stored immutable classification, must all have the sole orchestrator in their
// production call graph. Direct dispatches in those methods are additionally
// ordered after their local admission calls below; behavioral coverage of the
// four supported ingress surfaces lives in the gate matrix.
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
		exportedSQLDrive := fn.recv == "Engine" && ast.IsExported(fn.name) && reachesTargetDispatch(key, funcs, byName)
		if reason, exempt := ingressNonStatementDispatchExempt[key]; exempt {
			if reason == "" {
				t.Errorf("ingress exemption %s has no reason", key)
			}
			if !exportedSQLDrive {
				t.Errorf("ingress exemption %s no longer names an exported dispatch-reaching Engine method", key)
			}
			continue
		}
		if fn.recv == "Engine" && (fn.classifies || fn.executes || exportedSQLDrive) {
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
		for _, dispatchAt := range fn.dispatchAt {
			switch {
			case fn.executes && (fn.classAt == token.NoPos || fn.classAt > dispatchAt):
				t.Errorf("execute drive %s (%s) can dispatch before fresh class admission", drive, fn.file)
			case fn.classifies && (fn.policyAt == token.NoPos || fn.policyAt > dispatchAt):
				t.Errorf("classifying drive %s (%s) can dispatch before statement-policy admission", drive, fn.file)
			}
		}
	}
	for _, key := range []string{"Engine.wireAdmit", "Engine.WireParse", "Engine.WireExecutePortal"} {
		fn := funcs[key]
		if fn == nil || fn.grammarAt == token.NoPos {
			t.Errorf("%s does not run reported-grammar admission", key)
		}
	}
	if fn := funcs["Engine.WireParse"]; fn != nil && fn.grammarAt > fn.sizeAt {
		t.Error("Engine.WireParse moved reportedgrammar after sizecap")
	}
	if fn := funcs["Engine.WireExecutePortal"]; fn != nil && (fn.classAt == token.NoPos || fn.grammarAt > fn.classAt) {
		t.Error("Engine.WireExecutePortal must run reportedgrammar before execute-scoped authorizeunit")
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
					if fn.key != "sizeAdmissionStages" {
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
	if run := funcs["Engine.runSizeAdmission"]; run == nil || !run.calls["sizeAdmissionStages"] {
		t.Error("Engine.runSizeAdmission does not consume the canonical sizeAdmissionStages factory")
	}
	for guard, want := range wantCalls {
		if got := gotCalls[guard]; got != want {
			t.Errorf("production calls to %s = %d, want %d; adapter anchor moved or a bypass survived", guard, got, want)
		}
	}
}

func TestAdmissionChainRendering_ProductionAndRendererShareFactories(t *testing.T) {
	funcs, _ := loadIngressFuncs(t)
	wants := map[string]string{
		"Engine.runSizeAdmission":         "sizeAdmissionStages",
		"Engine.runWireGrammarAdmission":  "wireGrammarAdmissionStages",
		"Engine.runPrePolicyAdmission":    "profileAdmissionStages",
		"Engine.runPostPolicyAdmission":   "postPolicyAdmissionStages",
		"Engine.runProfileAdmission":      "profileAdmissionStages",
		"Engine.runClassAdmission":        "classAdmissionStages",
		"Engine.runSessionStateAdmission": "sessionStateAdmissionStages",
		"Engine.sessionStages":            "sessionAdmissionStages",
	}
	for production, factory := range wants {
		fn := funcs[production]
		if fn == nil || !fn.calls[factory] {
			t.Errorf("%s does not consume canonical factory %s", production, factory)
		}
		renderer := funcs["renderAdmissionChains"]
		if renderer == nil || !renderer.calls[factory] {
			t.Errorf("renderAdmissionChains does not consume production factory %s", factory)
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
