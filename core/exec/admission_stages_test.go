package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
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

func TestAuthorizeUnitAdapter_SameIdentityAsTheLegacyFloor(t *testing.T) {
	readerCtx := admission.Context{ReadOnly: true, MayWrite: false}
	editorCtx := admission.Context{ReadOnly: false, MayWrite: true}
	o := admission.Compose(authorizeUnitStage{})

	// A read passes on any policy — standing IS the read floor.
	stmt, err := Classify("SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, rerr := o.Run(NewLegacyFacts(stmt, 8, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the floor refused a read on a reader's policy")
	}

	// A write under a reader's policy: denied with the uniform identity.
	stmt, err = Classify("UPDATE t SET a = 1 WHERE id = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeUnitFloorPremise(stmt); err == nil {
		t.Fatal("the legacy floor admitted a write on a reader's policy — premise wrong")
	}
	rep, rerr = o.Run(NewLegacyFacts(stmt, 30, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted a write on a reader's policy")
	}
	if deny.Code != admission.CodeDenied {
		t.Fatalf("code = %s, want denied", deny.Code)
	}
	if deny.Detail != auth.ErrDenied.Error() {
		t.Fatalf("detail %q is not the sentinel's constant text %q — the uniform denial "+
			"never discloses existence", deny.Detail, auth.ErrDenied.Error())
	}

	// The same write on an editor's policy: admitted.
	rep, rerr = o.Run(NewLegacyFacts(stmt, 30, "", false, false), editorCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the floor refused a write on an editor's policy")
	}

	// A3's mutation: without the stage, the write runs on the reader's
	// policy — exactly what a drive that forgot the floor would do.
	rep, rerr = admission.Compose().Run(NewLegacyFacts(stmt, 30, "", false, false), readerCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}

// authorizeUnitFloorPremise asks the legacy floor through a reader policy,
// for the cell's premise assertion.
func authorizeUnitFloorPremise(stmt Statement) error {
	pol := UnitPolicy{ReadOnly: true, MayWrite: false}
	switch classToAction(stmt.Class) {
	case auth.ActionRead:
		return nil
	case auth.ActionWrite, auth.ActionDDL:
		if !pol.MayWrite {
			return auth.ErrDenied
		}
		return nil
	default:
		return auth.ErrDenied
	}
}

func newSessionStateStage() sessionStateStage {
	return sessionStateStage{parseSet: parseSet, parseReset: parseReset}
}

func controlFacts(t *testing.T, sql string) *LegacyFacts {
	t.Helper()
	stmt, err := Classify(sql, false)
	if err != nil {
		t.Fatalf("classifying %q: %v", sql, err)
	}
	return NewLegacyFactsForText(stmt, sql, len(sql))
}

func TestSessionStateAdapter_TwoGUCModelsDistinct(t *testing.T) {
	o := admission.Compose(newSessionStateStage())

	// THE TWO MODELS, same setting, different surfaces:
	//
	// temp_buffers is NOT on the pooled allowlist — SET LOCAL temp_buffers on a
	// pooled/session context refuses with the allowlist identity; on the
	// wire context (denylist model, editors get PostgreSQL as it is) the
	// same statement is admitted.
	setLocal := "SET LOCAL temp_buffers = '8MB'"
	pooledCtx := admission.Context{Phys: admission.PhysPooled, TxOpen: true}
	wireCtx := admission.Context{Phys: admission.PhysWire, TxOpen: true}

	st, err := parseSet(setLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := admitSet(st, true); err == nil {
		t.Fatal("the pooled allowlist admitted temp_buffers — premise wrong")
	}
	rep, rerr := o.Run(controlFacts(t, setLocal), pooledCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted SET LOCAL temp_buffers on the pooled model")
	}
	if deny.Code != admission.CodeSetGUCRefused {
		t.Fatalf("code = %s, want set-guc-refused (the allowlist arm)", deny.Code)
	}
	if !strings.Contains(deny.Detail, ErrSetGUCRefused.Error()) {
		t.Fatalf("detail %q does not carry the sentinel's text", deny.Detail)
	}

	rep, rerr = o.Run(controlFacts(t, setLocal), wireCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the wire DENYLIST refused temp_buffers — the denylist admits everything not named")
	}

	// THE MODELS DISAGREE IN BOTH DIRECTIONS, each with its own identity.
	//
	// search_path: the POOLED model refuses it in every form (a
	// grammar-changing setting desynchronizes the classifier) — the
	// wire model admits it for EDITORS (the backend is discarded at
	// close, the parsing hazard is pinned by the lease, and the
	// denylist does not name it) and refuses it for READERS (the
	// catalog-name shadowing the reader analysis relies on).
	pooledEditor := admission.Context{Phys: admission.PhysSession, TxOpen: true}
	wireEditor := admission.Context{Phys: admission.PhysWire, TxOpen: true, ReadOnly: false}
	readerWire := admission.Context{Phys: admission.PhysWire, TxOpen: true, ReadOnly: true}
	setSP := "SET LOCAL search_path = public"
	st, err = parseSet(setSP)
	if err != nil {
		t.Fatal(err)
	}
	if err := admitSet(st, true); err == nil {
		t.Fatal("the pooled allowlist admitted search_path — premise wrong (it is grammar-refused in every form)")
	}
	rep, rerr = o.Run(controlFacts(t, setSP), pooledEditor)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted search_path on the pooled model — the grammar arm")
	}
	if deny.Code != admission.CodeSetGUCRefused {
		t.Fatalf("pooled code = %s, want set-guc-refused (the grammar arm)", deny.Code)
	}
	// The wire EDITOR: admitted.
	rep, rerr = o.Run(controlFacts(t, setSP), wireEditor)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the wire denylist refused an editor's search_path — editors get PostgreSQL as it is")
	}
	// The wire READER: refused with the denylist's own identity.
	rep, rerr = o.Run(controlFacts(t, setSP), readerWire)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted a reader's search_path on the wire")
	}
	if deny.Code != admission.CodeWireSetRefused {
		t.Fatalf("code = %s, want wire-set-refused (the denylist arm)", deny.Code)
	}

	// A13's mutation: FLATTEN the two models — make the pooled path use
	// the denylist too — and one side's cells redden. Drive both arms
	// with one flattened stage and both must fail somewhere: here the
	// pooled temp_buffers case flips from refused to admitted, so the FIRST
	// cell above is the A13 proof (its premise is that the allowlist
	// refuses temp_buffers; flattening breaks it). The reverse flattening
	// (wire uses the allowlist) reddens the temp_buffers-on-wire case. Both
	// directions are pinned by the two cells above.

	// LOCK outside a transaction: the same rule on both models.
	lockCtx := admission.Context{Phys: admission.PhysSession, TxOpen: false}
	rep, rerr = o.Run(controlFacts(t, "LOCK TABLE t"), lockCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted LOCK outside a transaction")
	}
	if deny.Code != admission.CodeLockOutsideTx {
		t.Fatalf("code = %s, want lock-outside-tx", deny.Code)
	}

	// The RESET-on-pooled refusal and the RESET ALL wire refusal, identities
	// preserved.
	rep, rerr = o.Run(controlFacts(t, "RESET ALL"), admission.Context{Phys: admission.PhysWire})
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok = rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted RESET ALL on the wire")
	}
	if !strings.Contains(deny.Detail, ErrWireSetRefused.Error()) {
		t.Fatalf("detail %q does not carry the wire sentinel's text", deny.Detail)
	}

	// The chain without the stage: the refused SETs above run.
	rep, rerr = admission.Compose().Run(controlFacts(t, setLocal), pooledCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("the empty chain denied — the mutation's premise is wrong")
	}
}

// RESET parity on the wire: an admitted named RESET is ADMITTED (a
// fall-through in the adapter used to deny it), and a refused RESET
// keeps its identity.
func TestSessionStateAdapter_ResetParityOnTheWire(t *testing.T) {
	o := admission.Compose(newSessionStateStage())
	wireCtx := admission.Context{Phys: admission.PhysWire, TxOpen: true}

	// An admitted named RESET: the denylist does not name datestyle, and
	// the wire's backend is discarded at close — resetting a setting is
	// the caller's own business.
	st, err := parseReset("RESET datestyle")
	if err != nil {
		t.Fatal(err)
	}
	if err := admitWireReset(st, false); err != nil {
		t.Fatalf("premise wrong: the wire gate refused RESET datestyle: %v", err)
	}
	rep, rerr := o.Run(controlFacts(t, "RESET datestyle"), wireCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		deny, _ := rep.PrimaryDeny()
		t.Fatalf("the adapter refused an admitted named RESET on the wire: %s — %s",
			deny.Code, deny.Detail)
	}

	// A refused RESET: RESET ALL would reset the engine's own belts.
	if err := admitWireReset(resetStatement{All: true}, false); err == nil {
		t.Fatal("premise wrong: the wire gate admitted RESET ALL")
	}
	rep, rerr = o.Run(controlFacts(t, "RESET ALL"), wireCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the adapter admitted RESET ALL on the wire")
	}
	if deny.Code != admission.CodeWireSetRefused {
		t.Fatalf("code = %s, want wire-set-refused", deny.Code)
	}
	if !strings.Contains(deny.Detail, ErrWireSetRefused.Error()) {
		t.Fatalf("detail %q does not carry the wire sentinel's text", deny.Detail)
	}
}

// Foreign facts FAIL CLOSED: a facts implementation the legacy adapters
// cannot read must not turn a legacy refusal into an admission. The
// composing-fake cell validates a second STAGE; it never licensed a
// foreign FACTS carrier bypassing the legacy policy chain.
type foreignFacts struct {
	class admission.FactClass
}

func (f foreignFacts) Verb() string                  { return "UPDATE" }
func (f foreignFacts) Class() admission.FactClass    { return f.class }
func (foreignFacts) HasTopLevelWhere() bool          { return false }
func (foreignFacts) Mutations() []admission.Mutation { return nil }
func (foreignFacts) Calls() []admission.Call         { return nil }
func (foreignFacts) SetTarget() (string, bool, bool) { return "", false, false }
func (foreignFacts) TextLen() int                    { return 10 }

func TestForeignFacts_FailClosed(t *testing.T) {
	ctx := admission.Context{Phys: admission.PhysWire, TxOpen: true}

	for _, tc := range []struct {
		stage admission.Stage
		class admission.FactClass
	}{
		{guardWhereStage{}, admission.ClassWrite},
		{profileAdmitStage{ProfileV1Compat}, admission.ClassWrite},
		{authorizeUnitStage{}, admission.ClassWrite},
		{newSessionStateStage(), admission.ClassControl},
	} {
		// The fact's class must be one the stage APPLIES to (sessionstate
		// is ControlVerb-gated); absence-by-construction is applicability,
		// not a bypass — the fail-closed path is only reachable when the
		// stage is consulted.
		_, err := admission.Compose(tc.stage).Run(foreignFacts{class: tc.class}, ctx)
		if err == nil {
			t.Errorf("%s silently accepted foreign facts — a facts implementation the "+
				"stage cannot read must not bypass the legacy gate", tc.stage.Name())
			continue
		}
		if !strings.Contains(err.Error(), tc.stage.Name()) {
			t.Errorf("%s's fail-closed error does not name itself: %v", tc.stage.Name(), err)
		}
	}
}

// A4, drive-level: the pooled drive's chain is declared, split at the
// drive's I/O boundary exactly where the legacy order put it — the
// intake bound and capability profile BEFORE the actual-class
// authorization, the reader analysis and guard AFTER the unit policy.
// This cell pins both halves' ORDER; the wire drives must match the
// stage sequence (sizecap, profile … readeranalysis, guardwhere) with
// their own boundary placement.
func TestPooledDrive_ChainOrderIsDeclared(t *testing.T) {
	e := newChainTestEngine(t)
	in := admissionInputs{phys: admission.PhysPooled}

	pre := []admission.Stage{
		sizeCapStage{},
		profileAdmitStage{profile: e.profileFor(nil)},
	}
	post := []admission.Stage{
		readerAnalysisStage{userRoutines: nil},
		guardWhereStage{},
	}
	gotPre := admission.Compose(pre...).Order()
	gotPost := admission.Compose(post...).Order()
	if fmt.Sprint(gotPre) != fmt.Sprint([]string{"sizecap", "profile"}) {
		t.Fatalf("pre-policy half = %v, want [sizecap profile]", gotPre)
	}
	if fmt.Sprint(gotPost) != fmt.Sprint([]string{"readeranalysis", "guardwhere"}) {
		t.Fatalf("post-policy half = %v, want [readeranalysis guardwhere]", gotPost)
	}
	_ = in
}

// THE ORDERING DELTA'S EVIDENCE CELL. The design's ruling: profile
// admissibility PRECEDES reader analysis everywhere, because removing
// the UDF cannot make a compat-profile data-modifying CTE runnable. The
// pooled drive has always run admit-before-reader; the WIRE drives ran
// the reverse. This cell is written RED against the wire drives' current
// state (they have not been migrated yet) and goes green when Steps 4-5
// flip them — the named delta, its evidence kept separate from the
// preservation evidence, exactly as the task requires.
//
// The discriminator: a read-only compat-profile statement that violates
// BOTH stages — a data-modifying CTE that also calls a user-defined
// function. Under admit-first the refusal is statement-unsupported; under
// reader-first it is reader-advanced-pattern. When the wire drives are
// migrated, this cell's assertion CHANGES DIRECTION — the flip is
// recorded here rather than absorbed into a green suite.
func TestPooledDrive_OrderingAnswerMatchesTheRuling(t *testing.T) {
	// The pooled composition, evaluated for a reader unit whose statement
	// violates both stages: the profile gate must answer FIRST.
	stmt, err := Classify("WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT write_a_row() FROM x", false)
	if err != nil {
		t.Fatal(err)
	}
	set := &udfSet{bare: map[string]bool{"write_a_row": true}, qualified: map[string]bool{}}
	stages := []admission.Stage{
		sizeCapStage{},
		profileAdmitStage{profile: ProfileV1Compat},
		readerAnalysisStage{userRoutines: func() (*udfSet, error) { return set, nil }},
		guardWhereStage{},
	}
	rep, rerr := admission.Compose(stages...).Run(
		NewLegacyFactsForText(stmt, "WITH x AS ...", 80),
		admission.Context{Phys: admission.PhysSession, ReadOnly: true, TargetCaps: admission.CapRoutineCatalog, MaxStatementBytes: 1000})
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the double-violating statement was admitted")
	}
	if deny.Code != admission.CodeStatementUnsupported {
		t.Fatalf("the chain answered %s first — the ruling says profile admissibility "+
			"precedes reader analysis, because removing the UDF cannot make the "+
			"compat-profile CTE runnable: got %s (%s)",
			deny.Code, deny.Code, deny.Detail)
	}
}

// newChainTestEngine builds a bare engine for chain-level cells that need
// no store; the stages under test are pure.
func newChainTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := &Engine{profile: ProfileV1Compat, maxStatementBytes: DefaultMaxStatementBytes}
	return e
}

// The class-authorization precedence cell: a profile-invalid statement
// whose actual class is UNGRANTED must answer with the PROFILE's refusal
// — the legacy order's identity, not the class authorization's denial.
// The split exists so this caller's answer cannot change: the pre-policy
// half (sizecap, profile) runs before the drive's actual-class Authorize,
// exactly where the legacy gate ran.
func TestPooledDrive_ProfilePrecedesClassAuthorization(t *testing.T) {
	// BEGIN under the v1compat profile: the profile refuses it (control
	// statement). Its actual class is ClassControl, whose floor is DDL —
	// a caller without the DDL grant would get auth.ErrDenied IF the
	// class Authorize ran first; the legacy order answered the profile's
	// ErrStatementUnsupported.
	stmt, err := Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileV1Compat.admit(stmt, false); err == nil {
		t.Fatal("premise wrong: v1compat admitted BEGIN")
	}
	e := newChainTestEngine(t)
	preErr, opErr := e.runPrePolicyAdmission(context.Background(),
		admissionInputs{phys: admission.PhysPooled}, stmt, "BEGIN")
	if opErr != nil {
		t.Fatal(opErr)
	}
	if preErr == nil {
		t.Fatal("the pre-policy half admitted BEGIN — the profile stage is absent")
	}
	if !errors.Is(preErr, ErrStatementUnsupported) {
		t.Fatalf("the pre-policy refusal is %v — want the PROFILE's ErrStatementUnsupported; "+
			"a caller without the class grant must learn the profile's answer, which is "+
			"the legacy order's identity", preErr)
	}

	// And the converse discriminator: a profile-VALID statement passes the
	// pre-policy half, so the class Authorize's denial is what the caller
	// sees when their grant is missing — the two halves answering in the
	// legacy order.
	stmt, err = Classify("DROP TABLE t", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProfileV1Compat.admit(stmt, false); err != nil {
		t.Fatalf("premise wrong: v1compat refused DDL: %v", err)
	}
	preErr, opErr = e.runPrePolicyAdmission(context.Background(),
		admissionInputs{phys: admission.PhysPooled}, stmt, "DROP TABLE t")
	if opErr != nil {
		t.Fatal(opErr)
	}
	if preErr != nil {
		t.Fatalf("the pre-policy half refused a profile-valid statement: %v — the class "+
			"authorization's denial must be the one this caller sees, from the drive", preErr)
	}
}

// The pinned-transaction execution-state cell: it admits the
// session profile's control verbs on a POOLED call (the legacy gate's
// pinned != nil answer), and the unpinned call refuses them — with the
// transport staying PhysPooled in both, because the execution state is
// its own fact.
func TestPooledDrive_PinnedTxAdmitsControlVerbs(t *testing.T) {
	stmt, err := Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}
	// The session profile admits BEGIN on a session, refuses it off one.
	if err := ProfileSession.admit(stmt, true); err != nil {
		t.Fatalf("premise wrong: the session profile refused BEGIN on a session: %v", err)
	}
	if err := ProfileSession.admit(stmt, false); err == nil {
		t.Fatal("premise wrong: the session profile admitted BEGIN off a session")
	}

	e := newChainTestEngine(t)
	e.profile = ProfileSession
	_ = e

	stage := profileAdmitStage{profile: ProfileSession}

	// PINNED: the pooled call carrying a pinned transaction admits the
	// control verb — Phys stays Pooled, the execution state answers.
	pinnedCtx := admission.Context{Phys: admission.PhysPooled, PinnedTx: true}
	rep, rerr := admission.Compose(stage).Run(NewLegacyFactsForText(stmt, "BEGIN", 5), pinnedCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("a PINNED pooled call refused the session profile's control verb — " +
			"the legacy gate's pinned != nil answer admitted it")
	}

	// UNPINNED: refused, same transport.
	unpinnedCtx := admission.Context{Phys: admission.PhysPooled, PinnedTx: false}
	rep, rerr = admission.Compose(stage).Run(NewLegacyFactsForText(stmt, "BEGIN", 5), unpinnedCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("an UNPINNED pooled call admitted the control verb — off a transaction " +
			"it would run as text on a pooled connection and leave its state there")
	}
	if !errors.Is(reasonErr(deny), ErrStatementUnsupported) {
		t.Fatalf("the unpinned refusal lost its identity: %v", reasonErr(deny))
	}

	// And the transport-corruption guard: the pinned fact must NOT make
	// OnSession-applicable stages run — PinnedTx is execution state, not a
	// physical context. A session-only stage stays absent on the pinned
	// pooled call.
	onSessionStage := pinnedProbeStage{}
	rep, rerr = admission.Compose(onSessionStage).Run(NewLegacyFactsForText(stmt, "BEGIN", 5), pinnedCtx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.IsDenied() {
		t.Fatal("a PinnedTx pooled call satisfied an OnSession stage — the execution " +
			"state leaked into physical applicability")
	}
}

// pinnedProbeStage denies when consulted; it is applicable only on an
// affirmative session physical context.
type pinnedProbeStage struct{}

func (pinnedProbeStage) Name() string { return "pinnedprobe" }
func (pinnedProbeStage) ContextNeeds() admission.Needs {
	return admission.Needs{OnSession: true}
}
func (pinnedProbeStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeNoWhere}
}
func (pinnedProbeStage) Apply(admission.Facts, admission.Context) (admission.Contribution, error) {
	return admission.Deny(admission.Reason{Code: admission.CodeNoWhere, Continue: true}), nil
}

// The drive-wiring discriminators: the handoff from Engine.run into the
// split chain is proven through the REAL pooled flow — Execute, token,
// connection, statement — because a cell on the helper alone stays green
// when the call is severed from the dispatch.
//
// The discriminated callers:
//   - profile-INVALID + class-UNGRANTED: the pre-policy half must answer
//     ErrStatementUnsupported (the profile's identity), which proves the
//     pre-policy call runs BEFORE the class Authorize in run;
//   - profile-VALID + class-UNGRANTED: the class Authorize must answer
//     auth.ErrDenied, which proves the Authorize still runs after the
//     pre-half and its denial reaches the caller;
//   - PINNED vs UNPINNED pooled control verbs through
//     runPrePolicyAdmission with admissionInputs — proving the transfer
//     of the pinned fact from run's own argument into the chain context.
func TestPooledDrive_WiringPrecedesClassAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A reader-granted user (no DDL grant). BEGIN's actual class is
	// control, whose floor is DDL — ungranted for the reader.
	readerID, err := f.svc.CreateUser(ctx, f.rootTok, "wiring-reader", "wiring-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	readerTok, _, err := f.svc.Login(ctx, "wiring-reader", "wiring-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, readerID, f.connID, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	// Profile-INVALID + class-UNGRANTED: BEGIN under the default
	// (compat) profile is a profile refusal FIRST. The reader has no DDL
	// grant, so IF the class Authorize ran first the answer would be
	// auth.ErrDenied; the legacy order — and the split — answer the
	// profile's identity.
	if err := f.execErr(t, readerTok, "BEGIN"); !errors.Is(err, ErrStatementUnsupported) {
		t.Fatalf("profile-invalid + ungranted: err = %v, want ErrStatementUnsupported — the "+
			"pre-policy half is not wired before the class Authorize", err)
	}

	// Profile-VALID + class-UNGRANTED: DROP TABLE is admitted by the
	// compat profile, so the pre-half passes it and the class Authorize's
	// denial is what the caller sees.
	if err := f.execErr(t, readerTok, "DROP TABLE t"); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("profile-valid + ungranted: err = %v, want auth.ErrDenied — the class "+
			"Authorize is not wired after the pre-policy half", err)
	}

	// The audit records back the identity: the profile refusal and the
	// grant refusal both produced exec_rejected rows, and the store keeps
	// the refusal texts.
	n := f.auditCount(t, "exec_rejected")
	if n < 2 {
		t.Fatalf("audit rows = %d, want >= 2 — both refusals must record", n)
	}
}

// The pinned handoff: run's own pinned argument reaches the chain context
// through runPrePolicyAdmission — admitted when pinned, refused when not,
// with the transport staying pooled in both.
func TestPooledDrive_WiringPinnedTxHandoff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A session-profile connection, so the control-verb admission rides
	// the pinned fact. The fixture's engine default is compat; set the
	// connection's profile through the store the way production does.
	// (The fixture shares one connection; a second one keeps this cell
	// independent of the others' expectations.)
	connRow, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	connRow.Profile = string(ProfileSession)
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("setting the session profile on the fixture connection: %v", err)
	}

	stmt, err := Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}

	// PINNED through the helper the drive calls, with the inputs the
	// drive passes: admitted.
	preErr, opErr := f.eng.runPrePolicyAdmission(ctx,
		admissionInputs{connRow: connRow, phys: admission.PhysPooled, pinnedSet: true},
		stmt, "BEGIN")
	if opErr != nil {
		t.Fatal(opErr)
	}
	if preErr != nil {
		t.Fatalf("pinned pooled BEGIN refused through the wired helper: %v — the pinned "+
			"fact did not transfer", preErr)
	}

	// UNPINNED: refused with the profile identity.
	preErr, opErr = f.eng.runPrePolicyAdmission(ctx,
		admissionInputs{connRow: connRow, phys: admission.PhysPooled, pinnedSet: false},
		stmt, "BEGIN")
	if opErr != nil {
		t.Fatal(opErr)
	}
	if preErr == nil {
		t.Fatal("unpinned pooled BEGIN admitted — the control verb would run as text on " +
			"a pooled connection")
	}
	if !errors.Is(preErr, ErrStatementUnsupported) {
		t.Fatalf("unpinned refusal = %v, want ErrStatementUnsupported", preErr)
	}
}

// The profile-before-authorization ordering delta, through the real session
// flow. A read-only compat-profile unit whose data-modifying CTE violates both
// the profile and class floor must answer the profile's identity. The legacy
// session path authorized first and answered auth.ErrDenied.
func TestSessionDrive_ProfilePrecedesClassAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A reader-granted user on the compat-profile connection. The dm-CTE has
	// no UDF, so reader analysis passes and authorization is the competing gate.
	readerID, err := f.svc.CreateUser(ctx, f.rootTok, "delta-reader", "delta-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	readerTok, _, err := f.svc.Login(ctx, "delta-reader", "delta-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, readerID, f.connID, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	// The compat profile refuses the data-modifying CTE while its write class
	// exceeds the reader's grant. Profile-first answers statement-unsupported.
	sessID, err := f.eng.OpenSession(ctx, readerTok, f.connID, testIP)
	if err != nil {
		t.Fatal(err)
	}
	sql := "WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT coalesce(sum(x.id), 0) FROM x"
	_, err = f.eng.SessionExecute(ctx, readerTok, sessID, sql, testIP)
	if err == nil {
		t.Fatal("the double-violating statement was admitted")
	}
	if errors.Is(err, auth.ErrDenied) {
		t.Fatalf("the session drive answered with class authorization's identity: %v", err)
	}
	if !errors.Is(err, ErrStatementUnsupported) {
		t.Fatalf("the session drive answered %v — the ordering ruling says profile admissibility "+
			"precedes class authorization: a compat dm-CTE is refused as statement-unsupported, "+
			"not with the flat authorization denial", err)
	}
}

// A5, session-surface half: the same script through the POOLED drive
// answers the same. (The pooled path always ran admit-first, so its
// answer did not change — this cell pins that the two surfaces AGREE
// after the flip, the cross-surface parity Johno's requirement asserts.)
func TestSessionDrive_CrossSurfaceParityWithPooled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	readerID, err := f.svc.CreateUser(ctx, f.rootTok, "parity-reader", "parity-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	readerTok, _, err := f.svc.Login(ctx, "parity-reader", "parity-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, readerID, f.connID, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	sql := "WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT coalesce(sum(x.id), 0) FROM x"

	// Pooled (stateless Execute): the profile refuses the dm-CTE.
	pooledErr := f.execErr(t, readerTok, sql)

	// Session: the same statement, the same refusal identity.
	sessID, err := f.eng.OpenSession(ctx, readerTok, f.connID, testIP)
	if err != nil {
		t.Fatal(err)
	}
	_, sessErr := f.eng.SessionExecute(ctx, readerTok, sessID, sql, testIP)

	if !errors.Is(pooledErr, ErrStatementUnsupported) || !errors.Is(sessErr, ErrStatementUnsupported) {
		t.Fatalf("cross-surface parity broken: pooled = %v, session = %v — the same script on "+
			"the same connection must produce the same refusal identity on both surfaces",
			pooledErr, sessErr)
	}
}

// The wire-simple profile-before-authorization cell, at chain level. The
// front-door live-wire suite exercises the migrated gate end to end; this cell
// isolates the identity collision and pins the wire composition's order.
func TestWireSimpleDrive_ProfilePrecedesClassAuthorization(t *testing.T) {
	stmt, err := Classify("WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT coalesce(sum(x.id), 0) FROM x", false)
	if err != nil {
		t.Fatal(err)
	}
	stages := []admission.Stage{
		sizeCapStage{},
		profileAdmitStage{profile: ProfileV1Compat},
		readerAnalysisStage{userRoutines: nil},
		authorizeUnitStage{},
		guardWhereStage{},
	}
	rep, rerr := admission.Compose(stages...).Run(
		NewLegacyFactsForText(stmt, "WITH x AS ...", 90),
		admission.Context{Phys: admission.PhysWire, ReadOnly: true, MaxStatementBytes: 1000})
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, ok := rep.PrimaryDeny()
	if !ok {
		t.Fatal("the double-violating statement was admitted")
	}
	if deny.Code != admission.CodeStatementUnsupported {
		t.Fatalf("the wire chain answered %s — profile admissibility precedes class authorization "+
			"on every surface; auth.ErrDenied must not hide the profile refusal", deny.Code)
	}
}

// The PinnedTx truthfulness cell: the production derivation
// sessionAdmissionCtx maps txOpen truthfully onto PinnedTx — a session
// call OUTSIDE a transaction must report false, and PhysSession/PhysWire
// already supplies profile onSession; a constant true is false state a
// future stage could consume incorrectly. The cell invokes the production
// helper so a regression to constant true REDs.
func TestSessionDrive_PinnedTxIsTruthful(t *testing.T) {
	stmt, err := Classify("BEGIN", false)
	if err != nil {
		t.Fatal(err)
	}
	facts := NewLegacyFactsForText(stmt, "BEGIN", 5)
	e := newChainTestEngine(t)
	connRow := &meta.Connection{Engine: engine.SQLite}
	s := &session{wire: true}
	pol := UnitPolicy{}

	// Outside a transaction: production PinnedTx must be FALSE.
	ctx := e.sessionAdmissionCtx(connRow, pol, s, false)
	if ctx.PinnedTx {
		t.Fatalf("production PinnedTx is true outside a transaction — derivation regressed to constant true")
	}
	rep, rerr := admission.Compose(pinnedTxProbe{}).Run(facts, ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, _ := rep.PrimaryDeny()
	if deny.Subject != "false" {
		t.Fatalf("outside a transaction, PinnedTx read as %s — the field must report "+
			"the transaction state truthfully", deny.Subject)
	}

	// Inside one: true.
	ctx = e.sessionAdmissionCtx(connRow, pol, s, true)
	if !ctx.PinnedTx {
		t.Fatalf("production PinnedTx is false inside a transaction")
	}
	rep, rerr = admission.Compose(pinnedTxProbe{}).Run(facts, ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	deny, _ = rep.PrimaryDeny()
	if deny.Subject != "true" {
		t.Fatalf("inside a transaction, PinnedTx read as %s", deny.Subject)
	}
}

// pinnedTxProbe reports the PinnedTx fact it was handed, for the
// truthfulness cells.
type pinnedTxProbe struct{}

func (pinnedTxProbe) Name() string { return "pinnedtxprobe" }
func (pinnedTxProbe) ContextNeeds() admission.Needs {
	return admission.Needs{ControlVerb: true}
}
func (pinnedTxProbe) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeNoWhere}
}
func (pinnedTxProbe) Apply(_ admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	return admission.Deny(admission.Reason{
		Code:    admission.CodeNoWhere,
		Subject: fmt.Sprint(ctx.PinnedTx),
	}), nil
}
