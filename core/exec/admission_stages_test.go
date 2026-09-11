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

func TestProfileAdmitAdapter_SameIdentityAsTheLegacyGate(t *testing.T) {
	pooledCtx := admission.Context{Phys: admission.PhysPooled}
	sessionCtx := admission.Context{Phys: admission.PhysSession}
	v1compat := profileAdmitStage{ProfileV1Compat}
	session := profileAdmitStage{ProfileSession}

	// The compat profile refuses control statements, on and off a session
	// alike — with the refusal text naming the verb.
	stmt, err := Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileV1Compat.admit(stmt, true); err == nil {
		t.Fatal("the legacy gate admitted BEGIN under v1compat — premise wrong")
	}
	for _, ctx := range []admission.Context{pooledCtx, sessionCtx} {
		rep, rerr := admission.Compose(v1compat).Run(NewLegacyFacts(stmt, 5, "", false, false), ctx)
		if rerr != nil {
			t.Fatal(rerr)
		}
		deny, ok := rep.PrimaryDeny()
		if !ok {
			t.Fatalf("the adapter admitted BEGIN under v1compat on %s", ctx.Phys)
		}
		if deny.Code != admission.CodeStatementUnsupported {
			t.Fatalf("code = %s, want statement-unsupported", deny.Code)
		}
		if !strings.Contains(deny.Detail, ErrStatementUnsupported.Error()) {
			t.Fatalf("detail %q does not carry the sentinel's text", deny.Detail)
		}
	}

	// The compat profile refuses data-modifying CTEs.
	stmt, err = Classify("WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileV1Compat.admit(stmt, true); err == nil {
		t.Fatal("the legacy gate admitted a dm-CTE under v1compat — premise wrong")
	}
	rep, rerr := admission.Compose(v1compat).Run(NewLegacyFacts(stmt, 60, "", false, false), sessionCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, ok := rep.PrimaryDeny(); !ok {
		t.Fatal("the adapter admitted a dm-CTE under v1compat")
	}

	// The session profile admits guarded dm-CTEs…
	stmt, err = Classify("WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileSession.admit(stmt, true); err != nil {
		t.Fatalf("the legacy gate refused a guarded dm-CTE on the session profile: %v", err)
	}
	rep, rerr = admission.Compose(session).Run(NewLegacyFacts(stmt, 70, "", false, false), sessionCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the adapter refused a guarded dm-CTE on the session profile")
	}

	// …and routes control by the physical context: BEGIN is admitted ON a
	// session and refused OFF one, with the same identity the legacy gate
	// produced for the same call.
	stmt, err = Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileSession.admit(stmt, false); err == nil {
		t.Fatal("the legacy gate admitted BEGIN off a session on the session profile — premise wrong")
	}
	rep, rerr = admission.Compose(session).Run(NewLegacyFacts(stmt, 5, "", false, false), pooledCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted BEGIN off a session on the session profile")
	}
	if !strings.Contains(deny.Detail, ErrStatementUnsupported.Error()) {
		t.Fatalf("detail %q does not carry the sentinel's text", deny.Detail)
	}
	rep, rerr = admission.Compose(session).Run(NewLegacyFacts(stmt, 5, "", false, false), sessionCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the adapter refused BEGIN on a session on the session profile")
	}

	// A3's mutation: without the stage, the compat profile's refusals
	// vanish — a chain that forgot it would run control and dm-CTEs.
	rep, rerr = admission.Compose().Run(NewLegacyFacts(stmt, 5, "", false, false), pooledCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}

func TestReaderAnalysisAdapter_SameIdentityAsTheLegacyStage(t *testing.T) {
	readerCtx := admission.Context{ReadOnly: true, Phys: admission.PhysWire, TargetCaps: admission.CapRoutineCatalog}
	writerCtx := admission.Context{ReadOnly: false}
	set := &udfSet{bare: map[string]bool{"write_a_row": true}, qualified: map[string]bool{}}

	stage := readerAnalysisStage{userRoutines: func() (*udfSet, error) { return set, nil }}
	o := admission.Compose(stage)

	// The DO arm: denied by verb, same sentinel.
	stmt, err := Classify("DO $$ BEGIN END $$", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr := o.Run(NewLegacyFacts(stmt, 20, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted a reader's DO block")
	}
	if deny.Code != admission.CodeReaderAdvancedPattern {
		t.Fatalf("code = %s, want reader-advanced-pattern", deny.Code)
	}

	// The bare-UDF arm: a call the target's catalog answers to.
	stmt, err = Classify("SELECT write_a_row()", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := readerCallCheck(stmt.Calls, set); err == nil {
		t.Fatal("the legacy check admitted a bare UDF call — premise wrong")
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 20, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted a bare UDF call")
	}
	if !strings.Contains(deny.Detail, ErrReaderAdvancedPattern.Error()) {
		t.Fatalf("detail %q does not carry the sentinel's text", deny.Detail)
	}

	// The qualified arm.
	stmt, err = Classify("SELECT app.write_a_row()", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 24, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, ok := rep.PrimaryDeny(); !ok {
		t.Fatal("the adapter admitted a schema-qualified UDF call")
	}

	// Catalog calls stay allowed — they are the language.
	stmt, err = Classify("SELECT count(*) FROM t", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 22, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the adapter refused an ordinary catalog-function query")
	}

	// APPLICABILITY, both halves: a writer's unit never consults the
	// stage; a target without the routine-catalog capability makes it
	// absent by construction (the legacy no-op arm).
	rep, rerr = o.Run(NewLegacyFacts(stmt, 22, "", false, false), writerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the reader stage ran on a WRITER's unit — editors get PostgreSQL as it is")
	}
	noCapCtx := admission.Context{ReadOnly: true, TargetCaps: 0}
	stmt, err = Classify("SELECT write_a_row()", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 20, "", false, false), noCapCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the reader stage denied on a target without the catalog capability — " +
			"its reader safety rests on the classifier and the driver's read-only transaction, by construction")
	}

	// THE OPERATIONAL SPLIT: a broken catalog read surfaces as an ERROR,
	// never as a denial — the split this seam exists to make.
	broken := readerAnalysisStage{userRoutines: func() (*udfSet, error) { return nil, errFakeCatalog }}
	_, rerr = admission.Compose(broken).Run(NewLegacyFacts(stmt, 20, "", false, false), readerCtx)
	if rerr == nil {
		t.Fatal("the stage's operational failure was swallowed — 'could not decide' must " +
			"reach the caller differently from 'refused'")
	}
	if !strings.Contains(rerr.Error(), "routine catalog could not be read") {
		t.Fatalf("the operational error does not carry the legacy text: %v", rerr)
	}
	rep2, _ := admission.Compose(broken).Run(NewLegacyFacts(stmt, 20, "", false, false), readerCtx)
	_ = rep2

	// A3's mutation: the chain without the stage admits the UDF call.
	rep, rerr = admission.Compose().Run(NewLegacyFacts(stmt, 20, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}

// errFakeCatalog is the operational failure the broken-catalog cell uses.
var errFakeCatalog = errFake{}

type errFake struct{}

func (errFake) Error() string { return "fake: catalog unreachable" }
