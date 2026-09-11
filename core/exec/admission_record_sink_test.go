package exec

import (
	"context"
	"fmt"
	"go/ast"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
)

type discoveredRefusalStage struct {
	name   string
	reason admission.Reason
}

// A production caller must not return a stage refusal directly. Helpers may
// propagate operational errors, while admission refusals leave through reject,
// rejectSession, or rejectRecordedAttempt so the audit consequence cannot be
// bypassed by adding an early return at one drive.
func TestAdmissionRefusalPathsCannotBypassProductionSink(t *testing.T) {
	funcs, _ := loadIngressFuncs(t)
	admissionRuns := map[string]bool{
		"runSizeAdmission": true, "runWireGrammarAdmission": true,
		"runPrePolicyAdmission": true, "runPostPolicyAdmission": true,
		"runProfileAdmission": true, "runClassAdmission": true,
		"runSessionStateAdmission": true, "runSessionAdmission": true,
		"runReadOnlyEnforcementAdmission": true,
	}
	checked := 0
	for _, fn := range funcs {
		if fn.key == "Engine.wrapReadOnly" {
			continue // enforcement is terminalized by executeUnit after the attempt exists
		}
		var refusalVars []string
		ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) == 0 {
				return true
			}
			for _, rhs := range assign.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || !admissionRuns[ingressCallName(call)] {
					continue
				}
				if id, ok := assign.Lhs[0].(*ast.Ident); ok {
					refusalVars = append(refusalVars, id.Name)
				}
			}
			return true
		})
		for _, refusalVar := range refusalVars {
			checked++
			ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
				ret, ok := node.(*ast.ReturnStmt)
				if !ok || !astNodeNames(ret, refusalVar) {
					return true
				}
				if !astNodeCallsAny(ret, "reject", "rejectSession", "rejectRecordedAttempt") &&
					!returnImmediatelyFollowsSink(fn.decl.Body, ret, "auditBounded") {
					t.Errorf("%s (%s) returns admission refusal %s without a production record sink", fn.key, fn.file, refusalVar)
				}
				return true
			})
		}
	}
	if checked < 12 {
		t.Fatalf("only %d production admission-result paths checked; discovery became vacuous", checked)
	}
}

func returnImmediatelyFollowsSink(body *ast.BlockStmt, target *ast.ReturnStmt, sink string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return !found
		}
		for i, stmt := range block.List {
			if stmt == target && i > 0 && astNodeCallsAny(block.List[i-1], sink) {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

func astNodeNames(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func astNodeCallsAny(node ast.Node, names ...string) bool {
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && want[ingressCallName(call)] {
			found = true
		}
		return !found
	})
	return found
}

func (s discoveredRefusalStage) Name() string                  { return s.name }
func (s discoveredRefusalStage) ContextNeeds() admission.Needs { return admission.Needs{} }
func (s discoveredRefusalStage) DenyCodes() []admission.Code   { return []admission.Code{s.reason.Code} }
func (s discoveredRefusalStage) Apply(admission.Facts, admission.Context) (admission.Contribution, error) {
	return admission.Deny(s.reason), nil
}

// A11 derives its denial codes from the same live registration as A23. Each
// code is driven through the orchestrator and the production refusal sink, then
// observed in the audit store. The structural test above separately proves that
// production admission-result paths cannot bypass that sink.
func TestEveryRegisteredAdmissionDenialCodeIsRecordable(t *testing.T) {
	for _, registration := range admissionRegistry.Registered() {
		for _, code := range registration.DenyCod {
			registration, code := registration, code
			t.Run(registration.Name+"/"+string(code), func(t *testing.T) {
				f := newFixture(t)
				ident, err := f.svc.ValidateToken(context.Background(), f.rootTok)
				if err != nil {
					t.Fatal(err)
				}
				marker := fmt.Sprintf("registered refusal %s/%s", registration.Name, code)
				reason := admission.Reason{
					Code: code, Class: admission.ClassPermission, Detail: marker, Continue: true,
				}
				refusal, opErr := f.eng.evaluateChain(
					[]admission.Stage{discoveredRefusalStage{name: registration.Name, reason: reason}},
					NewLegacyFactsForText(Statement{}, "", 0), admission.Context{})
				if opErr != nil || refusal == nil {
					t.Fatalf("orchestrator = refusal %v, operational %v", refusal, opErr)
				}
				if err := f.eng.reject(context.Background(), ident, f.connID, testIP, "SELECT 1", refusal); err == nil {
					t.Fatal("record sink returned no refusal")
				}
				audits := f.audits(t, "exec_rejected")
				if len(audits) != 1 || !strings.Contains(audits[0].Detail, marker) {
					t.Fatalf("recorded refusals = %+v, want one containing %q", audits, marker)
				}
			})
		}
	}
}
