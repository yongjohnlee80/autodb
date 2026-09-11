package exec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
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
		// FOREIGN FACTS FAIL CLOSED: this stage enforces the WHERE guard
		// through the classifier's own verdict shape, and a facts
		// implementation it cannot read is one it cannot enforce. A
		// silent no-op would let an incompatible facts carrier bypass
		// the guard entirely, so the stage says it broke — loudly.
		return admission.NoContribution(), fmt.Errorf("admission: guardwhere requires the legacy facts representation; got %T", facts)
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
		// FOREIGN FACTS FAIL CLOSED — silently skipping the capability
		// gate would admit whatever the foreign facts describe.
		return admission.NoContribution(), fmt.Errorf("admission: profile requires the legacy facts representation; got %T", facts)
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

// authorizeUnitStage is the class floor: a statement's own class,
// authorized against the policy verdict the engine already resolved. The
// same rule auth.decide applies, asked of a snapshot rather than by
// reading again — and AUTHORITY IS DELIBERATELY NOT CACHED HERE: the
// drive re-resolves per Execute and hands this stage the fresh
// snapshot, so a grant revoked between Parse and Execute refuses at the
// next Execute, never at a remembered verdict.
type authorizeUnitStage struct{}

func (authorizeUnitStage) Name() string { return "authorizeunit" }

func (authorizeUnitStage) ContextNeeds() admission.Needs { return admission.Needs{} }

func (authorizeUnitStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeDenied}
}

// Apply maps the statement's class onto the action it needs and asks the
// policy snapshot. The denial is the UNIFORM authorization refusal —
// never disclosing existence — and its identity is auth.ErrDenied, the
// sentinel every caller's handling is written against.
func (authorizeUnitStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	lf, ok := facts.(*LegacyFacts)
	if !ok {
		// FOREIGN FACTS FAIL CLOSED — silently skipping the class floor
		// would authorize whatever class the foreign facts claim.
		return admission.NoContribution(), fmt.Errorf("admission: authorizeunit requires the legacy facts representation; got %T", facts)
	}
	if err := classToActionFloor(lf.stmt.Class, ctx); err != nil {
		return admission.Deny(admission.Reason{
			Code:     admission.CodeDenied,
			Detail:   err.Error(),
			Continue: true,
		}), nil
	}
	return admission.NoContribution(), nil
}

// classToActionFloor asks the policy snapshot the statement's class floor
// — the same decision authorizeUnit makes, expressed against the seam's
// Context snapshot. Read is the floor for standing at all; write and DDL
// need the write floor; an unmapped class is refused rather than waved
// through.
func classToActionFloor(c Class, ctx admission.Context) error {
	switch classToAction(c) {
	case auth.ActionRead:
		return nil
	case auth.ActionWrite, auth.ActionDDL:
		if !ctx.MayWrite {
			return auth.ErrDenied
		}
		return nil
	default:
		return auth.ErrDenied
	}
}

// sessionStateStage is the SET/LOCK gate: one stage, TWO GUC MODELS,
// selected by the physical context. The pooled and RPC-session paths run
// the ALLOWLIST (benign GUCs + mandatory LOCAL, because their connection
// outlives the caller and state would leak to the next pool user); the
// wire path runs the DENYLIST (the backend is discarded at close, so the
// leak hazard does not exist and editors get PostgreSQL as it is).
//
// The two models are APPLICABILITY SETS OF ONE STAGE, not two stages: the
// A13 mutation is that flattening them fails a cell, and keeping the
// dispatch in one place is what lets the matrix state the divergence once
// while the drives state it never.
type sessionStateStage struct {
	// parseSet and parseReset are supplied as closures for symmetry with
	// the other adapters; they are the package's own pure functions and
	// the tests drive the real ones.
	parseSet   func(sqlText string) (setStatement, error)
	parseReset func(sqlText string) (resetStatement, error)
}

func (sessionStateStage) Name() string { return "sessionstate" }

func (sessionStateStage) ContextNeeds() admission.Needs {
	return admission.Needs{ControlVerb: true}
}

func (s sessionStateStage) DenyCodes() []admission.Code {
	return []admission.Code{
		admission.CodeSetGUCRefused,
		admission.CodeSetNotLocal,
		admission.CodeSetOutsideTx,
		admission.CodeLockOutsideTx,
		admission.CodeWireSetRefused,
		admission.CodeStatementUnsupported,
	}
}

// Apply decides one control verb's session-state admissibility. The
// facts carry the parsed set shape when the caller parsed one (the SET
// and LOCK arms); the context carries the physical context that selects
// the GUC model, the read-only policy, and whether the caller's
// transaction is open.
func (s sessionStateStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	lf, ok := facts.(*LegacyFacts)
	if !ok {
		// FOREIGN FACTS FAIL CLOSED — silently skipping the session-state
		// gate would admit SET/LOCK shapes the gate exists to refuse.
		return admission.NoContribution(), fmt.Errorf("admission: sessionstate requires the legacy facts representation; got %T", facts)
	}
	switch lf.stmt.Verb {
	case "LOCK":
		if err := admitLock(ctx.TxOpen); err != nil {
			return denyFrom(err), nil
		}
		return admission.NoContribution(), nil
	case "SET":
		st, err := s.parseSet(lf.sqlText)
		if err != nil {
			return denyFrom(err), nil
		}
		if ctx.Phys == admission.PhysWire {
			if err := admitWireSet(st, ctx.ReadOnly, ctx.TxOpen); err != nil {
				return denyFrom(err), nil
			}
			return admission.NoContribution(), nil
		}
		if err := admitSet(st, ctx.TxOpen); err != nil {
			return denyFrom(err), nil
		}
		return admission.NoContribution(), nil
	case "RESET":
		st, err := s.parseReset(lf.sqlText)
		if err != nil {
			return denyFrom(err), nil
		}
		if ctx.Phys == admission.PhysWire {
			if err := admitWireReset(st, ctx.ReadOnly); err != nil {
				return denyFrom(err), nil
			}
			// ADMITTED on the wire: a named RESET the denylist does not
			// refuse. Falling through to the pooled-path refusal below
			// would deny every valid named RESET — the wire's backend is
			// discarded at close, so resetting a setting is the caller's
			// own business, which is why the two models diverge here too.
			return admission.NoContribution(), nil
		}
		// Off the wire, RESET has no meaning: pooled connections carry no
		// session-level state a caller may have set (only SET LOCAL is
		// admitted, and it reverts with the transaction) — the engine's
		// own refusal, identity preserved.
		return denyFrom(fmt.Errorf("%w: RESET has no meaning on a pooled connection; only a wire session holds settings",
			ErrStatementUnsupported)), nil
	default:
		// A control verb the gate has no rule for: the engine's own
		// catch-all refusal, identity preserved.
		return denyFrom(fmt.Errorf("%w: %s", ErrStatementUnsupported, lf.stmt.Verb)), nil
	}
}

// denyFrom maps a legacy gate error onto its Reason. Every sentinel the
// SET/LOCK gates produce carries a distinct CODE, and the error's own
// text — which the callers' handling is written against — rides verbatim
// in the Detail. An unmapped error is mapped to statement-unsupported
// rather than dropped: the refusal must reach the client with SOME
// identity, never silently.
func denyFrom(err error) admission.Contribution {
	switch {
	case errorsIs(err, ErrSetGUCRefused):
		return admission.Deny(admission.Reason{Code: admission.CodeSetGUCRefused, Detail: err.Error(), Continue: true})
	case errorsIs(err, ErrSetNotLocal):
		return admission.Deny(admission.Reason{Code: admission.CodeSetNotLocal, Detail: err.Error(), Continue: true})
	case errorsIs(err, ErrSetOutsideTx):
		return admission.Deny(admission.Reason{Code: admission.CodeSetOutsideTx, Detail: err.Error(), Continue: true})
	case errorsIs(err, ErrLockOutsideTx):
		return admission.Deny(admission.Reason{Code: admission.CodeLockOutsideTx, Detail: err.Error(), Continue: true})
	case errorsIs(err, ErrWireSetRefused):
		return admission.Deny(admission.Reason{Code: admission.CodeWireSetRefused, Detail: err.Error(), Continue: true})
	default:
		return admission.Deny(admission.Reason{Code: admission.CodeStatementUnsupported, Detail: err.Error(), Continue: true})
	}
}

// errorsIs and sqlText plumbing: the adapter reads the statement's SQL
// text for the SET/RESET parse. The classifier's verdict does not carry
// the raw text (it carries the shape), so the text rides the LegacyFacts
// — supplied by the drive, which holds it.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// reasonErr maps a Reason back onto the LEGACY SENTINEL its code stands
// for, so the drives' rejection paths keep the identity — including the
// errors.Is chain — the callers' error handling is written against. The
// compatibility surface this phase preserves is the sentinel WRAP, not
// merely the text: tests and callers ask errors.Is(err, sentinel), so
// the returned error must wrap the sentinel with the arm's own message.
//
// The adapters compose their Details as "<sentinel text><arm detail>",
// so the reconstruction is the sentinel wrapped with everything the arm
// appended beyond the sentinel's own text — the exact shape
// fmt.Errorf("%w: ...", sentinel) produced before the move.
func reasonErr(r admission.Reason) error {
	sentinel, ok := legacySentinelFor(r.Code)
	if !ok {
		return fmt.Errorf("exec: unmapped admission code %q (detail %q) — a refusal with no "+
			"identity mapping must never reach the client", r.Code, r.Detail)
	}
	suffix := strings.TrimPrefix(r.Detail, sentinel.Error())
	if suffix == r.Detail && r.Detail != "" {
		// The detail is not sentinel-prefixed (a future stage's shape);
		// keep the sentinel wrap and append the detail as context.
		return fmt.Errorf("%w: %s", sentinel, r.Detail)
	}
	if suffix == "" {
		return sentinel
	}
	return fmt.Errorf("%w%s", sentinel, suffix)
}

// legacySentinelFor is the code→sentinel table: the identity each
// refusal keeps.
func legacySentinelFor(c admission.Code) (error, bool) {
	switch c {
	case admission.CodeScriptTooLarge:
		return ErrScriptTooLarge, true
	case admission.CodeNoWhere:
		return ErrNoWhere, true
	case admission.CodeStatementUnsupported:
		return ErrStatementUnsupported, true
	case admission.CodeReaderAdvancedPattern:
		return ErrReaderAdvancedPattern, true
	case admission.CodeSetGUCRefused:
		return ErrSetGUCRefused, true
	case admission.CodeSetNotLocal:
		return ErrSetNotLocal, true
	case admission.CodeSetOutsideTx:
		return ErrSetOutsideTx, true
	case admission.CodeLockOutsideTx:
		return ErrLockOutsideTx, true
	case admission.CodeWireSetRefused:
		return ErrWireSetRefused, true
	case admission.CodeDenied:
		return auth.ErrDenied, true
	case admission.CodeReadOnlyUnenforceable:
		return ErrReadOnlyUnenforceable, true
	}
	return nil, false
}
