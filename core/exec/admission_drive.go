package exec

import (
	"context"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/meta"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
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

// These factories are the production compositions. Keep stage construction
// here: the drives and the chain renderer both consume these exact slices, so
// the committed rendering changes when production order or membership changes.
func sizeAdmissionStages() []admission.Stage {
	return []admission.Stage{sizeCapStage{}}
}

func wireGrammarAdmissionStages(verify func() error) []admission.Stage {
	return []admission.Stage{reportedGrammarStage{verify: verify}}
}

func postPolicyAdmissionStages(userRoutines func() (*udfSet, error)) []admission.Stage {
	return []admission.Stage{
		readerAnalysisStage{userRoutines: userRoutines},
		guardWhereStage{},
	}
}

// profileAdmissionStages is the capability composition every control route
// consults. The procedural stage rides WITH the profile rather than in a
// separate chain because the control routes have no later boundary — the
// pooled control path's only gate is this one — so a placement rule composed
// anywhere else would be absent exactly where it is needed.
func profileAdmissionStages(profile Profile) []admission.Stage {
	return []admission.Stage{profileAdmitStage{profile: profile}, proceduralBlockStage{}}
}

// proceduralAdmissionStages is what a DISPATCHED control statement must clear
// beyond its placement: the reader analysis and the class floor.
//
// The control routes skip the session chain deliberately — a transaction verb
// never reaches a target, so gating it as though it might is meaningless — and
// that skip was written when every control verb either became an engine action
// or was refused. A procedural verb is neither: it is forwarded as text. So
// this chain asks the two questions the skip dropped, rather than inheriting a
// bypass written for a different kind of statement.
func proceduralAdmissionStages(userRoutines func() (*udfSet, error)) []admission.Stage {
	return []admission.Stage{
		readerAnalysisStage{userRoutines: userRoutines},
		authorizeUnitStage{},
	}
}

func classAdmissionStages() []admission.Stage {
	return []admission.Stage{authorizeUnitStage{}}
}

func sessionStateAdmissionStages() []admission.Stage {
	return []admission.Stage{newSessionStateStage()}
}

func readOnlyEnforcementStages() []admission.Stage {
	return []admission.Stage{readOnlyEnforcementStage{}}
}

func sessionAdmissionStages(profile Profile, userRoutines func() (*udfSet, error)) []admission.Stage {
	return []admission.Stage{
		profileAdmitStage{profile: profile},
		// Composed here too, not only with the control routes: the extended
		// protocol gates a procedural verb through THIS chain (it is not owned
		// control, so Parse takes the ordinary route), and a placement rule
		// missing from a chain that can see the verb is a hole by omission.
		// It is ControlVerb-applicable, so it is absent for every ordinary
		// statement by construction rather than by an early return.
		proceduralBlockStage{},
		readerAnalysisStage{userRoutines: userRoutines},
		authorizeUnitStage{},
		guardWhereStage{},
	}
}

// runSizeAdmission evaluates the intake bound before classification. A zero
// Statement is intentional: this stage needs only the raw text length.
func (e *Engine) runSizeAdmission(phys admission.PhysicalCtx, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(Statement{}, sqlText, len(sqlText))
	return e.evaluateChain(sizeAdmissionStages(), facts,
		admission.Context{Phys: phys, MaxStatementBytes: e.maxStatementBytes})
}

// runWireGrammarAdmission re-reads PostgreSQL's captured ParameterStatus state
// before each statement-bearing wire operation. It performs no target query,
// so the client-visible prepared-statement and portal namespaces stay untouched.
func (e *Engine) runWireGrammarAdmission(s *session) (error, error) {
	pc := s.pinnedConn()
	if pc == nil {
		return nil, nil
	}
	reporter, ok := e.reporterFor(pc).(golibpg.ParameterStatusReporter)
	if !ok {
		return nil, fmt.Errorf("exec: pinned PostgreSQL session has no reported parameter status capability")
	}
	statuses := reporter.ReportedParameterStatuses()
	stages := wireGrammarAdmissionStages(func() error {
		return (postgresDialect{}).VerifyReportedGrammar(func(name string) string { return statuses[name] })
	})
	return e.evaluateChain(stages, NewLegacyFactsForText(Statement{}, "", 0),
		admission.Context{Phys: admission.PhysWire})
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
	stages := profileAdmissionStages(e.profileFor(in.connRow))
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
	stages := postPolicyAdmissionStages(func() (*udfSet, error) {
		return e.userRoutines(ctx, in.connRow)
	})
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
	return AdmissionError(deny), nil
}

// runProfileAdmission keeps control routing in its drive while moving the
// capability decision behind the same profile stage ordinary statements use.
func (e *Engine) runProfileAdmission(profile Profile, phys admission.PhysicalCtx, pinnedBackend bool, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain(profileAdmissionStages(profile), facts,
		admission.Context{Profile: string(profile), Phys: phys, PinnedBackend: pinnedBackend})
}

// runClassAdmission asks the class-floor stage against one freshly resolved
// policy snapshot. The caller owns resolution timing and never caches policy in
// the chain; WireExecutePortal invokes this anew for every Execute.
func (e *Engine) runClassAdmission(pol UnitPolicy, phys admission.PhysicalCtx, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain(classAdmissionStages(), facts,
		admission.Context{Phys: phys, ReadOnly: pol.ReadOnly, MayWrite: pol.MayWrite})
}

// runSessionStateAdmission preserves the drive's routing and authority floors
// while moving the SET/RESET/LOCK policy decision behind its adapter.
func (e *Engine) runSessionStateAdmission(pol UnitPolicy, phys admission.PhysicalCtx, txOpen bool, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	return e.evaluateChain(sessionStateAdmissionStages(), facts,
		admission.Context{Phys: phys, ReadOnly: pol.ReadOnly, MayWrite: pol.MayWrite, TxOpen: txOpen})
}

func (e *Engine) runReadOnlyEnforcementAdmission(phys admission.PhysicalCtx, available bool) (error, error) {
	var caps admission.TargetCaps
	if available {
		caps |= admission.CapTxReadOnly
	}
	return e.evaluateChain(readOnlyEnforcementStages(), NewLegacyFactsForText(Statement{}, "", 0), admission.Context{
		Phys: phys, ReadOnly: true, TargetCaps: caps,
	})
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
	return sessionAdmissionStages(e.profileFor(connRow), func() (*udfSet, error) {
		return e.userRoutines(ctx, connRow)
	})
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
		Profile: string(e.profileFor(connRow)),
		Phys:    phys,
		// The BACKEND fact, read from the session rather than inferred from
		// the transport: a wire session against a target that does not speak
		// the PostgreSQL wire protocol pins nothing, and its statements run
		// on a pooled target connection like any other.
		PinnedBackend: s.pinnedConn() != nil,
		ReadOnly:      pol.ReadOnly,
		MayWrite:      pol.MayWrite,
		TxOpen:        txOpen,
		PinnedTx:      txOpen, // the pinned fact is the transaction state, truthfully:
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

// runProceduralAdmission evaluates the dispatch gates for DO and CALL on a
// wire session. The context comes from the production derivation the ordinary
// session path uses, so the read-only verdict, the write floor and the
// target's capabilities are the same facts, resolved the same way — a
// hand-built context here would be a second opinion about the same unit.
func (e *Engine) runProceduralAdmission(ctx context.Context, s *session, pol UnitPolicy, connRow *meta.Connection, txOpen bool, stmt Statement, sqlText string) (error, error) {
	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	stages := proceduralAdmissionStages(func() (*udfSet, error) {
		return e.userRoutines(ctx, connRow)
	})
	return e.evaluateChain(stages, facts, e.sessionAdmissionCtx(connRow, pol, s, txOpen))
}

// renderAdmissionChains prints the current production stage compositions in a
// reviewable drive map. Every O stage list comes from the same factory used by
// production. D and R annotations are explanatory topology, intentionally
// review-maintained rather than claimed as source-derived control-flow proof.
func renderAdmissionChains() string {
	chain := func(stages []admission.Stage) string {
		rendered := admission.Compose(stages...).Render()
		for _, stage := range stages {
			if profile, ok := stage.(profileAdmitStage); ok {
				return strings.Replace(rendered, profile.Name(), fmt.Sprintf("%s(%s)", profile.Name(), profile.profile), 1)
			}
		}
		return rendered
	}
	var b strings.Builder
	for _, profile := range []Profile{ProfileV1Compat, ProfileSession} {
		fmt.Fprintf(&b, "profile=%s\n", profile)
		fmt.Fprintf(&b, "  pooled/ordinary: O1{%s} -> D{Classify} -> O2{%s} -> D{actual-class grant + fresh policy} -> O3{%s} -> D{attempt} -> O4{%s if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}\n",
			chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(postPolicyAdmissionStages(nil)), chain(readOnlyEnforcementStages()))
		fmt.Fprintf(&b, "  pooled/control: O1{%s} -> D{Classify} -> O2{%s} -> D{off-session control refusal; no later boundary}\n",
			chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)))
		fmt.Fprintf(&b, "  rpc-session/ordinary: O1{%s} -> D{Classify; control branches away} -> O2{%s} -> D{tx-state -> attempt} -> O3{%s if target capability absent} -> D{per-statement read-only wrap or compatibility audit -> dispatch}\n",
			chain(sizeAdmissionStages()), chain(sessionAdmissionStages(profile, nil)), chain(readOnlyEnforcementStages()))
		fmt.Fprintf(&b, "  rpc-session/control-stateful: O1{%s} -> D{Classify -> control route} -> O2{%s} -> D{control floor} -> O3{%s} -> D{tx-state} -> O4{%s} -> D{RPC SET admin floor -> attempt -> per-statement read-only wrap -> dispatch}\n",
			chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(classAdmissionStages()), chain(sessionStateAdmissionStages()))
		fmt.Fprintf(&b, "  rpc-session/control-transaction: O1{%s} -> D{Classify -> control route} -> O2{%s} -> D{control floor -> ParseTxControl -> handleTxControl}\n",
			chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)))
		fmt.Fprintf(&b, "  wire-simple/decoded-ordinary: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{%s} -> D{Classify; control branches away} -> O2{%s} -> D{tx-state -> attempt} -> O3{%s if target capability absent} -> D{per-statement read-only wrap or refusal -> decoded dispatch}\n",
			chain(sizeAdmissionStages()), chain(sessionAdmissionStages(profile, nil)), chain(readOnlyEnforcementStages()))
		fmt.Fprintf(&b, "  wire-simple/decoded-control: D{reported grammar bypass: no pinned PostgreSQL backend} -> O1{%s} -> D{Classify -> control route} -> O2{%s} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{%s} -> D{stateful: attempt -> per-statement read-only wrap -> decoded dispatch; transaction: ParseTxControl -> handleTxControl}\n",
			chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(sessionStateAdmissionStages()))
		fmt.Fprintf(&b, "  wire-simple/raw-ordinary: O1{%s} -> O2{%s} -> D{split + all-statements gate} -> O3{%s} -> D{Classify; control branches away} -> O4{%s} -> D{join -> attempts -> raw-segment read-only wrap -> dispatch}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()), chain(sizeAdmissionStages()), chain(sessionAdmissionStages(profile, nil)))
		fmt.Fprintf(&b, "  wire-simple/raw-control-stateful: O1{%s} -> O2{%s} -> D{split + all-statements gate} -> O3{%s} -> D{Classify -> control route} -> O4{%s} -> D{control floor -> tx-state} -> O5{%s} -> D{join -> attempt -> raw-segment read-only wrap -> dispatch}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()), chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(sessionStateAdmissionStages()))
		fmt.Fprintf(&b, "  wire-simple/raw-control-procedural: O1{%s} -> O2{%s} -> D{split + all-statements gate} -> O3{%s} -> D{Classify -> control route} -> O4{%s} -> D{control floor -> procedural branch} -> O5{%s} -> D{tx-state -> join -> attempt -> raw-segment dispatch -> routine-cache invalidation}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()), chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(proceduralAdmissionStages(nil)))
		fmt.Fprintf(&b, "  wire-decoded-or-extended/control-procedural: D{decoded control route, or extended Execute of a deferred control -> wireControl} -> O1{%s} -> D{control floor -> procedural branch} -> O2{%s} -> D{tx-state -> attempt -> dispatch -> routine-cache invalidation}\n",
			chain(profileAdmissionStages(profile)), chain(proceduralAdmissionStages(nil)))
		fmt.Fprintf(&b, "  wire-simple/raw-control-transaction: O1{%s} -> O2{%s} -> D{split + all-statements gate} -> O3{%s} -> D{Classify -> owned-control route} -> O4{%s} -> D{control floor -> join -> attempt -> wireControl} -> O5{%s} -> D{control floor -> ParseTxControl -> handleTxControl}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()), chain(sizeAdmissionStages()), chain(profileAdmissionStages(profile)), chain(profileAdmissionStages(profile)))
		fmt.Fprintf(&b, "  wire-extended/Parse-ordinary: R{segment read-only wrap acquired at entry} -> O1{%s} -> O2{%s} -> D{Classify; empty/control branches away} -> O3{%s} -> D{statement resource reservation -> target Parse}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()), chain(sessionAdmissionStages(profile, nil)))
		fmt.Fprintf(&b, "  wire-extended/Parse-control: R{segment read-only wrap acquired at entry} -> O1{%s} -> O2{%s} -> D{Classify -> deferred control: reserve/store only; admission waits for Execute}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(sizeAdmissionStages()))
		fmt.Fprintf(&b, "  wire-extended/Execute-ordinary: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{%s} -> O2{%s} -> D{tx-state + inspect segment wrap} -> O3{%s if reader wrap absent} -> D{attempt -> ExecuteOp}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(classAdmissionStages()), chain(readOnlyEnforcementStages()))
		fmt.Fprintf(&b, "  wire-extended/Execute-deferred-control: R{segment read-only wrap acquired/retained at entry} -> D{portal lookup -> fresh policy} -> O1{%s} -> D{release segment wrap -> wireControl} -> O2{%s} -> D{control floor -> stateful or transaction branch -> stateful only: tx-state} -> O3{%s} -> D{stateful: attempt -> dispatch; transaction: ParseTxControl -> handleTxControl}\n",
			chain(wireGrammarAdmissionStages(nil)), chain(profileAdmissionStages(profile)), chain(sessionStateAdmissionStages()))
		fmt.Fprintf(&b, "  wire-startup/GUC: D{authenticate -> authorize target -> exposure -> reserve -> pin/checkout grammar -> UTF-8 lease -> fresh policy} -> O1{%s} -> D{target SET; repeat per GUC}\n",
			chain(sessionStateAdmissionStages()))
	}
	return b.String()
}
