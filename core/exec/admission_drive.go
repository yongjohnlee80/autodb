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

// runSizeAdmission evaluates the intake bound before classification. A zero
// Statement is intentional: this stage needs only the raw text length.
func (e *Engine) runSizeAdmission(phys admission.PhysicalCtx, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(Statement{}, sqlText, len(sqlText))
	return e.evaluateChain([]admission.Stage{sizeCapStage{}}, facts,
		admission.Context{Phys: phys, MaxStatementBytes: e.maxStatementBytes})
}

// runPrePolicyAdmission evaluates the capability stage whose legacy position
// is BEFORE the drive's class authorization and unit-policy resolution. The
// ORDER the legacy path ran is load-bearing — the profile's ErrStatementUnsupported for
// an unsupported statement must precede the actual-class Authorize's
// auth.ErrDenied, because a caller without the class grant who sends a
// profile-invalid statement must learn the PROFILE's answer, not the
// grant's (found in the step-3 review; the reorder changed refusal
// identity for exactly that caller).
//
// Operational error: a stage broke; refusal: the legacy identity.
func (e *Engine) runPrePolicyAdmission(ctx context.Context, in admissionInputs, stmt Statement, sqlText string) (error, error) {
	actx := admission.Context{
		Profile:  string(e.profileFor(in.connRow)),
		Phys:     in.phys,
		PinnedTx: in.pinnedSet,
	}
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	stages := []admission.Stage{
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
		Profile:    string(e.profileFor(in.connRow)),
		Phys:       in.phys,
		PinnedTx:   in.pinnedSet,
		ReadOnly:   pol.ReadOnly,
		MayWrite:   pol.MayWrite,
		TxOpen:     in.txOpen,
		TargetCaps: caps,
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

// runProfileAdmission keeps control routing in its drive while moving the
// capability decision behind the same profile stage ordinary statements use.
func (e *Engine) runProfileAdmission(profile Profile, phys admission.PhysicalCtx, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain([]admission.Stage{profileAdmitStage{profile: profile}}, facts,
		admission.Context{Profile: string(profile), Phys: phys})
}

// runClassAdmission asks the class-floor stage against one freshly resolved
// policy snapshot. The caller owns resolution timing and never caches policy in
// the chain; WireExecutePortal invokes this anew for every Execute.
func (e *Engine) runClassAdmission(pol UnitPolicy, phys admission.PhysicalCtx, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain([]admission.Stage{authorizeUnitStage{}}, facts,
		admission.Context{Phys: phys, ReadOnly: pol.ReadOnly, MayWrite: pol.MayWrite})
}

// runSessionStateAdmission preserves the drive's routing and authority floors
// while moving the SET/RESET/LOCK policy decision behind its adapter.
func (e *Engine) runSessionStateAdmission(pol UnitPolicy, phys admission.PhysicalCtx, txOpen bool, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain([]admission.Stage{newSessionStateStage()}, facts,
		admission.Context{Phys: phys, ReadOnly: pol.ReadOnly, MayWrite: pol.MayWrite, TxOpen: txOpen})
}

// sessionStages is the session drive's composition: exactly the gates its
// legacy path ran, in the declared order. The session path's class floor
// is the SNAPSHOT check (authorizeUnit against the already-resolved
// policy) rather than the token-level grant lookup the pooled path runs,
// so the snapshot-floor stage IS composed here.
//
// THE ORDERING DELTAS LAND HERE. The legacy session path ran reader analysis
// and class authorization BEFORE the profile gate; the declared order runs the
// profile first. A read-only compat data-modifying CTE now answers
// statement-unsupported instead of reader-advanced-pattern when it calls a
// UDF, or auth.ErrDenied when it does not. One profile-first decision creates
// both intentional identity changes; the gate matrix records the disclosure
// tradeoff and why neither can reach the corpus manifest.
// The session, wire-simple and extended-Parse discriminator cells pin each
// collision independently from the preservation evidence.
func (e *Engine) sessionStages(ctx context.Context, connRow *meta.Connection) []admission.Stage {
	return []admission.Stage{
		profileAdmitStage{profile: e.profileFor(connRow)},
		readerAnalysisStage{userRoutines: func() (*udfSet, error) {
			return e.userRoutines(ctx, connRow)
		}},
		authorizeUnitStage{},
		guardWhereStage{},
	}
}

// sessionAdmissionCtx is the production Context construction for the
// session drive. Extracted so the PinnedTx derivation (txOpen → PinnedTx,
// truthfully) is testable through the seam the drive actually calls — a
// hand-constructed Context in the test would stay green when the
// derivation regresses to constant true.
func (e *Engine) sessionAdmissionCtx(connRow *meta.Connection, pol UnitPolicy, s *session, txOpen bool) admission.Context {
	var caps admission.TargetCaps
	if connRow.Engine.HasRoutineCatalog() {
		caps |= admission.CapRoutineCatalog
	}
	phys := admission.PhysSession
	if s.wire {
		phys = admission.PhysWire
	}
	return admission.Context{
		Profile:  string(e.profileFor(connRow)),
		Phys:     phys,
		ReadOnly: pol.ReadOnly,
		MayWrite: pol.MayWrite,
		TxOpen:   txOpen,
		PinnedTx: txOpen, // the pinned fact is the transaction state, truthfully:
		// PhysSession/PhysWire already supplies profile onSession; PinnedTx
		// answers only whether THIS execution carries a pinned transaction,
		// and a session outside one must not report true — a future stage
		// reading the contract would be lied to.
		TargetCaps: caps,
	}
}

// runSessionAdmission evaluates the session drive's chain for one
// statement. The session path resolves its policy ONCE per unit before
// this call (the fresh-per-unit read the never-cache rule requires) and
// passes the snapshot; the whole gate sequence is pure, so it is ONE
// evaluation, not split — the drive's I/O (policy resolve, transaction
// authority preflight) all precedes it.
func (e *Engine) runSessionAdmission(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection, txOpen bool, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain(e.sessionStages(ctx, connRow), facts, e.sessionAdmissionCtx(connRow, pol, s, txOpen))
}
