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

// runAdmission composes the declared chain for these inputs, evaluates
// one statement's facts, and returns the legacy error identity for a
// refusal — or nil when the statement is admitted.
//
// The stage set is the same for every surface (size before understanding,
// capability before reader analysis, the guard after the class floor);
// what differs per drive is the INPUTS, and the inputs are data, not
// code paths. The session-state stage is composed only where control
// verbs reach it — on the pooled path the profile layer refuses every
// control statement, so the stage would never be consulted and is
// omitted by construction.
//
// err is ALWAYS an operational failure (a stage broke); a refusal is the
// returned legacy error with err == nil. The two must never be confused:
// "the pipeline could not decide" and "the statement is refused" reach
// the client differently, and the split is the seam's central promise.
func (e *Engine) runAdmission(ctx context.Context, in admissionInputs, stages []admission.Stage, stmt Statement, sqlText string, setOK setNameLocal) (error, error) {
	// The per-evaluation stage inputs, as closures over values the drive
	// already holds. The reader stage's catalog read is engine I/O bound
	// to this connection; the profile comes from this connection's row.
	var caps admission.TargetCaps
	if in.connRow.Engine.HasRoutineCatalog() {
		caps |= admission.CapRoutineCatalog
	}

	facts := NewLegacyFactsForText(stmt, sqlText, len(sqlText))
	if setOK.ok {
		facts = NewLegacyFacts(stmt, len(sqlText), setOK.name, setOK.local, true)
		facts.sqlText = sqlText
	}

	actx := admission.Context{
		Profile:           string(e.profileFor(in.connRow)),
		Phys:              in.phys,
		ReadOnly:          in.pol.ReadOnly,
		MayWrite:          in.pol.MayWrite,
		TxOpen:            in.txOpen,
		Aborted:           false,
		TargetCaps:        caps,
		MaxStatementBytes: e.maxStatementBytes,
	}

	rep, rerr := admission.Compose(stages...).Run(facts, actx)
	if rerr != nil {
		// A stage broke: operational, not a refusal.
		return nil, rerr
	}
	if !rep.IsDenied() {
		return nil, nil
	}
	deny, _ := rep.PrimaryDeny()
	return reasonErr(deny), nil
}

// pooledStages is the pooled drive's composition: exactly the gates its
// legacy path ran, in the declared order — size before understanding,
// capability before reader analysis, the guard last. The class floor is
// the token-level grant Authorize the drive performs itself (a class
// action lookup the chain cannot see), so the snapshot-floor stage is
// NOT composed here: adding it would tighten behaviour, which phase 1
// forbids.
func (e *Engine) pooledStages(ctx context.Context, connRow *meta.Connection) []admission.Stage {
	return []admission.Stage{
		sizeCapStage{},
		profileAdmitStage{profile: e.profileFor(connRow)},
		readerAnalysisStage{userRoutines: func() (*udfSet, error) {
			return e.userRoutines(ctx, connRow)
		}},
		guardWhereStage{},
	}
}

// setNameLocal is the parsed set-statement shape, for the drives that
// parsed one before admission (the SET/LOCK arms on the session paths).
type setNameLocal struct {
	name  string
	local bool
	ok    bool
}
