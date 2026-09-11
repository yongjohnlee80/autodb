package exec

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/core/admission"
)

// The legacy guards, adapted to the admission seam. The ADAPTERS are
// constructed here, in exec, closing over the engine's own unexported
// functions: the identity mapping (errors.Is against the exported
// sentinels) happens in the closure, and the leaf package names nothing
// from this package. The logic is not rewritten — each adapter holds the
// guard itself, maps its error identity onto a Reason per the minimal deny
// table, and stops. The mapping table is the one place identity could
// silently change, so every row carries the sentinel it maps and the cell
// that pins it.
//
// A stage is absent by construction where it is inapplicable (the seam's
// Needs and TargetCaps declarations), and it declares every Code it can
// deny with — mandatory disclosure, rejected at evaluation time if a
// denial arrives undeclared.

// sizeCapStage is the intake bound: reject oversized text BEFORE
// classification, so the audit record always equals what ran — never
// execute an unaudited tail. It is the first stage in every chain and the
// same rule at every one of the engine's former check sites.
type sizeCapStage struct{}

func (sizeCapStage) Name() string { return "sizecap" }

func (sizeCapStage) ContextNeeds() admission.Needs { return admission.Needs{} }

func (sizeCapStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeScriptTooLarge}
}

// Apply enforces the bound the drive supplies through Context. The Reason
// preserves the sentinel's meaning word-for-word: an oversized script is
// refused before understanding, and the identity is the compatibility
// surface.
func (sizeCapStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	if ctx.MaxStatementBytes > 0 && facts.TextLen() > ctx.MaxStatementBytes {
		return admission.Deny(admission.Reason{
			Code:     admission.CodeScriptTooLarge,
			Subject:  fmt.Sprintf("%d bytes", facts.TextLen()),
			Detail:   ErrScriptTooLarge.Error(),
			Continue: true,
		}), nil
	}
	return admission.NoContribution(), nil
}

// guardWhereStage is the WHERE guard: a mutation that can reach every row
// must say which rows it means, at every depth — top level and inside
// data-modifying CTEs alike. The rule is one sentence and the legacy
// implementation is the whole of it; the adapter maps its two refusal
// arms onto one code (the sentinel ErrNoWhere is the compatibility
// surface for both) and preserves the error text verbatim in the Detail.
type guardWhereStage struct{}

func (guardWhereStage) Name() string { return "guardwhere" }

func (guardWhereStage) ContextNeeds() admission.Needs { return admission.Needs{} }

func (guardWhereStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeNoWhere}
}

// Apply runs the legacy guard against the classifier verdict the facts
// carry. The guard's own error is the identity: ErrNoWhere, with the
// nested arm's verb and depth in the text.
func (guardWhereStage) Apply(facts admission.Facts, _ admission.Context) (admission.Contribution, error) {
	lf, ok := facts.(*LegacyFacts)
	if !ok {
		return admission.NoContribution(), nil
	}
	if err := guardWhere(lf.stmt); err != nil {
		return admission.Deny(admission.Reason{
			Code:     admission.CodeNoWhere,
			Subject:  lf.stmt.Verb,
			Detail:   err.Error(),
			Continue: true,
		}), nil
	}
	return admission.NoContribution(), nil
}

// profileAdmitStage is the capability profile: what this connection and
// surface may run. It is the ONLY place a control statement's
// admissibility is decided, and the ONE stage whose answer the phase-1
// ordering ruling changes on the wire paths — profile admissibility
// precedes reader analysis everywhere, because removing the UDF cannot
// make a compat-profile data-modifying CTE runnable.
//
// The onSession fact the legacy gate took as a parameter is a PHYSICAL
// CONTEXT fact, so it comes from Context: the pooled path passes
// PhysPooled (its admit site computed pinned != nil; the drives will
// keep passing the session's own physical context when a transaction is
// pinned), and every session-shaped surface passes its affirmative
// context. The stage asks; the drive supplies.
type profileAdmitStage struct {
	profile Profile
}

func (p profileAdmitStage) Name() string { return "profile" }

func (p profileAdmitStage) ContextNeeds() admission.Needs { return admission.Needs{} }

func (p profileAdmitStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeStatementUnsupported}
}

// Apply runs the legacy Profile.admit with the physical context's answer
// to "is the caller the session path". The error identity —
// ErrStatementUnsupported, with the verb and the refusal's reason in the
// text — is the compatibility surface callers' error handling is written
// against; it rides verbatim in the Detail.
func (p profileAdmitStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	lf, ok := facts.(*LegacyFacts)
	if !ok {
		return admission.NoContribution(), nil
	}
	onSession := ctx.Phys == admission.PhysSession || ctx.Phys == admission.PhysWire
	if err := p.profile.admit(lf.stmt, onSession); err != nil {
		return admission.Deny(admission.Reason{
			Code:     admission.CodeStatementUnsupported,
			Subject:  lf.stmt.Verb,
			Detail:   err.Error(),
			Continue: true,
		}), nil
	}
	return admission.NoContribution(), nil
}

// readerAnalysisStage is the editors-first rule: reader units may not run
// advanced patterns — user-defined function calls, procedural blocks —
// that could carry a write or a state change past the read-only wrap.
//
// THE CATALOG'S I/O FAILURE IS AN OPERATIONAL ERROR, NOT A REFUSAL. This
// is the split the seam exists to make: 'the target's routine catalog
// could not be read' means the STAGE could not decide, and it returns the
// error rather than a denial — exactly as the legacy code's caller's
// rejection path treated it, but now structurally distinguishable from the
// three policy arms (qualified UDF call, bare UDF call, DO/CALL verb).
//
// Applicability is declarative: ReadOnlyUnit (the stage runs for reader
// units only). The routine-catalog requirement is PER-ARM, not
// stage-wide: the DO/CALL verb arm denies on every target (the legacy
// code's verb switch precedes the catalog check), while the CALL arms
// consult the catalog only where one exists — a target without the
// capability skips the call analysis by construction, exactly the
// legacy no-op arm, whose reader safety rests on the classifier and the
// driver's read-only transaction.
type readerAnalysisStage struct {
	// userRoutines resolves the target's user-defined routine set, or
	// fails operationally. The ENGINE supplies the closure — the catalog
	// read is engine I/O, and the stage holds the function rather than
	// the engine itself so the composition stays a value.
	userRoutines func() (*udfSet, error)
}

func (readerAnalysisStage) Name() string { return "readeranalysis" }

func (readerAnalysisStage) ContextNeeds() admission.Needs {
	return admission.Needs{ReadOnlyUnit: true}
}

func (readerAnalysisStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeReaderAdvancedPattern}
}

// Apply decides the reader's advanced-pattern question for one statement.
// The DO/CALL arm is denied by verb; the call arms are denied against the
// target's own routine set.
func (s readerAnalysisStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	switch facts.Verb() {
	case "DO", "CALL":
		return admission.Deny(admission.Reason{
			Code:     admission.CodeReaderAdvancedPattern,
			Subject:  facts.Verb(),
			Detail:   fmt.Errorf("%w: %s", ErrReaderAdvancedPattern, facts.Verb()).Error(),
			Continue: true,
		}), nil
	}
	if len(facts.Calls()) == 0 {
		return admission.NoContribution(), nil
	}
	// THE CATALOG ARM IS CAPABILITY-GATED, per-arm: without a routine
	// catalog the call analysis is absent by construction (the legacy
	// no-op), and the DO/CALL arm above still denied — the verb switch
	// preceded the catalog check in the legacy code too.
	if !ctx.TargetCaps.Has(admission.CapRoutineCatalog) {
		return admission.NoContribution(), nil
	}
	set, err := s.userRoutines()
	if err != nil {
		// The stage BROKE — the catalog could not be read. Not a refusal:
		// the caller must be able to tell 'refused' from 'could not
		// decide', and the legacy text rides in the wrap.
		return admission.NoContribution(), fmt.Errorf("%w: the target's routine catalog could not be read (%v)",
			ErrReaderAdvancedPattern, err)
	}
	for _, c := range facts.Calls() {
		switch {
		case c.Schema == "pg_catalog" || c.Schema == "information_schema":
			continue
		case c.Schema != "":
			return admission.Deny(admission.Reason{
				Code:     admission.CodeReaderAdvancedPattern,
				Subject:  c.Schema + "." + c.Name,
				Detail:   fmt.Errorf("%w: %s.%s()", ErrReaderAdvancedPattern, c.Schema, c.Name).Error(),
				Continue: true,
			}), nil
		case set.bare[c.Name]:
			return admission.Deny(admission.Reason{
				Code:     admission.CodeReaderAdvancedPattern,
				Subject:  c.Name,
				Detail:   fmt.Errorf("%w: %s()", ErrReaderAdvancedPattern, c.Name).Error(),
				Continue: true,
			}), nil
		}
	}
	return admission.NoContribution(), nil
}
