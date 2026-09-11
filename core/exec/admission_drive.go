package exec

import (
	"context"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The drives' side of the seam: composing the one declared chain for one
// evaluation, running it, and mapping a denial back onto the legacy
// sentinel identity the rejection records and the callers' error handling
// are written against.
//
// THE GATE MOVES; THE RECORDS DO NOT. The chain decides; every refusal
// still exits through the drive's own reject/rejectSession so the audit
// record, its identity and its position are exactly what they were. This
// file contains no recording logic of its own — that is the behaviour
// preservation the whole phase stands on.

// admissionInputs is everything the chain needs that the DRIVE holds:
// the connection row (profile, engine capabilities), the execution-unit
// policy snapshot, the caller's transaction state, and the physical
// context the statement arrived through.
type admissionInputs struct {
	connRow   *meta.Connection
	pol       UnitPolicy
	phys      admission.PhysicalCtx
	txOpen    bool
	pinnedSet bool // the pooled drive's onSession fact: a pinned transaction
}

// runPrePolicyAdmission evaluates the stages whose legacy position is
// BEFORE the drive's class authorization and unit-policy resolution:
// the intake bound and the capability profile. The ORDER the legacy
// path ran is load-bearing — the profile's ErrStatementUnsupported for
// an unsupported statement must precede the actual-class Authorize's
// auth.ErrDenied, because a caller without the class grant who sends a
// profile-invalid statement must learn the PROFILE's answer, not the
// grant's (found in the step-3 review; the reorder changed refusal
// identity for exactly that caller).
//
// Operational error: a stage broke; refusal: the legacy identity.
func (e *Engine) runPrePolicyAdmission(ctx context.Context, in admissionInputs, stmt Statement, sqlText string) (error, error) {
	actx := admission.Context{
		Profile:           string(e.profileFor(in.connRow)),
		Phys:              in.phys,
		PinnedTx:          in.pinnedSet,
		MaxStatementBytes: e.maxStatementBytes,
	}
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	stages := []admission.Stage{
		sizeCapStage{},
		profileAdmitStage{profile: e.profileFor(in.connRow)},
	}
	return e.evaluateChain(stages, facts, actx)
}

// runPostPolicyAdmission evaluates the stages whose legacy position is
// AFTER the drive's unit-policy resolution: the reader analysis (needs
// the policy snapshot's read-only verdict and the connection's target
// capabilities) and the WHERE guard. The policy snapshot arrives as
// input — resolved FRESH by the drive at every unit, never cached here.
func (e *Engine) runPostPolicyAdmission(ctx context.Context, in admissionInputs, pol UnitPolicy, stmt Statement, sqlText string) (error, error) {
	var caps admission.TargetCaps
	if in.connRow.Engine.HasRoutineCatalog() {
		caps |= admission.CapRoutineCatalog
	}
	actx := admission.Context{
		Profile:           string(e.profileFor(in.connRow)),
		Phys:              in.phys,
		PinnedTx:          in.pinnedSet,
		ReadOnly:          pol.ReadOnly,
		MayWrite:          pol.MayWrite,
		TxOpen:            in.txOpen,
		TargetCaps:        caps,
		MaxStatementBytes: e.maxStatementBytes,
	}
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	stages := []admission.Stage{
		readerAnalysisStage{userRoutines: func() (*udfSet, error) {
			return e.userRoutines(ctx, in.connRow)
		}},
		guardWhereStage{},
	}
	return e.evaluateChain(stages, facts, actx)
}

// evaluateChain runs one composed slice and maps the outcome onto the
// drive's two-error convention: refusal (legacy identity, op == nil) or
// operational break (refusal == nil, op != nil). The invariant is
// exclusive — exactly one is non-nil — and held by exactly this helper.
func (e *Engine) evaluateChain(stages []admission.Stage, facts admission.Facts, actx admission.Context) (error, error) {
	rep, rerr := admission.Compose(stages...).Run(facts, actx)
	if rerr != nil {
		return nil, rerr
	}
	if !rep.IsDenied() {
		return nil, nil
	}
	deny, _ := rep.PrimaryDeny()
	return reasonErr(deny), nil
}
