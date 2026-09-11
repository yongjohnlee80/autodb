package exec

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
)

// The adapters' identity cells (A3): for every error identity the legacy
// guard produced, the adapter refuses the same input with the same
// identity — the Reason's Code and Detail — and the mutation is the stage's
// deletion from the chain (that identity stops being produced).
//
// The mapping table is the reviewed artifact: each row names the sentinel
// it maps, and the cell pins both halves — the legacy guard's own error
// for the same input, and the Reason the adapter produces.

// factsFor builds LegacyFacts from real classification, so the cells
// exercise the actual classifier output rather than hand-built verdicts.
func factsFor(t *testing.T, sql string, textLen int) *LegacyFacts {
	t.Helper()
	stmt, err := Classify(sql, false)
	if err != nil {
		t.Fatalf("classifying %q: %v", sql, err)
	}
	return NewLegacyFacts(stmt, textLen, "", false, false)
}

func TestSizeCapAdapter_SameIdentityAsTheLegacyCheck(t *testing.T) {
	ctx := admission.Context{MaxStatementBytes: 100}

	// The same input through the legacy check…
	stmt, err := Classify("SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = stmt

	// …and through the adapter. Oversized text:
	facts := NewLegacyFacts(stmt, 200, "", false, false)
	rep, rerr := admission.Compose(sizeCapStage{}).Run(facts, ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("oversized text was admitted")
	}
	if deny.Code != admission.CodeScriptTooLarge {
		t.Fatalf("code = %s, want script-too-large", deny.Code)
	}
	// The DETAIL is the sentinel's own constant text — the identity the
	// callers' error handling is written against.
	if !strings.Contains(deny.Detail, ErrScriptTooLarge.Error()) {
		t.Fatalf("detail %q does not carry the sentinel's text %q", deny.Detail, ErrScriptTooLarge.Error())
	}

	// Under the bound: admitted.
	facts = NewLegacyFacts(stmt, 10, "", false, false)
	rep, rerr = admission.Compose(sizeCapStage{}).Run(facts, ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("text under the bound was refused")
	}

	// A3's mutation: delete the stage from the chain (compose nothing) and
	// the identity stops being produced — the orchestrator admits, which
	// is exactly what a drive that forgot the stage would do.
	rep, rerr = admission.Compose().Run(facts.WithLen(200), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}

// WithLen returns a copy of the facts carrying a different text length, so
// the A3 mutation can drive the same oversized input past a chain that
// lacks the stage.
func (l *LegacyFacts) WithLen(n int) *LegacyFacts {
	cp := *l
	cp.textLen = n
	return &cp
}

func TestGuardWhereAdapter_SameIdentityAsTheLegacyGuard(t *testing.T) {
	ctx := admission.Context{}
	o := admission.Compose(guardWhereStage{})

	// Top-level arm: UPDATE without WHERE. The legacy guard and the
	// adapter refuse with the same sentinel.
	stmt, err := Classify("UPDATE t SET a = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if guardWhere(stmt) == nil {
		t.Fatal("the legacy guard admitted a top-level UPDATE without WHERE — the cell's premise is wrong")
	}
	rep, rerr := o.Run(NewLegacyFacts(stmt, len("UPDATE t SET a = 1"), "", false, false), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted what the legacy guard refuses")
	}
	if deny.Code != admission.CodeNoWhere {
		t.Fatalf("code = %s, want mutation-without-predicate", deny.Code)
	}
	if !strings.Contains(deny.Detail, ErrNoWhere.Error()) {
		t.Fatalf("detail %q does not carry ErrNoWhere's text", deny.Detail)
	}

	// Nested arm: a data-modifying CTE's inner mutation without WHERE.
	nestedSQL := "WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x"
	stmt, err = Classify(nestedSQL, false)
	if err != nil {
		t.Fatal(err)
	}
	if guardWhere(stmt) == nil {
		t.Fatal("the legacy guard admitted a nested mutation without WHERE — the cell's premise is wrong")
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, len(nestedSQL), "", false, false), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted a nested mutation the legacy guard refuses")
	}
	if deny.Code != admission.CodeNoWhere {
		t.Fatalf("nested arm code = %s, want mutation-without-predicate", deny.Code)
	}

	// The guarded half: a mutation WITH a predicate passes.
	stmt, err = Classify("UPDATE t SET a = 1 WHERE id = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if guardWhere(stmt) != nil {
		t.Fatal("the legacy guard refused a guarded UPDATE — the cell's premise is wrong")
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 30, "", false, false), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the adapter refused a guarded mutation")
	}

	// A3's mutation: the chain without the stage admits what the guard
	// refuses — a drive that forgot the stage would run an unguarded
	// full-table mutation.
	rep, rerr = admission.Compose().Run(NewLegacyFacts(stmt, 30, "", false, false), ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}
