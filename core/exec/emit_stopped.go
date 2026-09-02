package exec

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// EmitStopped is what WireQuery returns when the CONSUMER stopped the output —
// the loop's emit callback failed (a write failed, a budget cut the stream) —
// after a statement had been dispatched. It carries what the engine itself
// ESTABLISHED about the statement whose frames the client did not receive, so
// the loop reports the same truth the audit row records instead of inferring
// one from a transaction status byte (PR #52 MF16: `I` is left behind by a
// committed autocommit AND by a failed one; the status alone proves nothing).
//
// The loop obtains it with errors.As and asks Arm() what became of the
// statement. Arm encodes the six arms and their ORDER once, here, so the loop
// and the audit cannot disagree about them:
//
//  1. !Executed — no statement ran (the empty query): there are NO effects to
//     describe, whatever the transaction status says — an empty query inside
//     BEGIN leaves T and still has nothing pending. Checked before every
//     effect arm for exactly that reason (lector #60 r1 MF2).
//  2. TargetErr != nil — the target's ErrorResponse for that statement passed
//     through the emitter: it FAILED; in autocommit nothing was kept.
//  3. TxStatus == 'T' — the client's transaction is still open: the effects
//     are PENDING; COMMIT or ROLLBACK still decides them.
//  4. TxStatus == 'E' — the transaction is aborted: the effects will be rolled
//     back.
//  5. Outcome == StatusOK — the engine observed the statement COMPLETE (its
//     CommandComplete arrived; the owned-control and decoded paths always have
//     the whole result before the first emit): the effects were kept.
//  6. otherwise — the tail was drained UNOBSERVED (golib stops delivery at the
//     first emit error and drains to ReadyForQuery without it): whether the
//     target kept the statement is not known to the front door. "Unresolved",
//     never a guess.
//
// Unwrap returns Cause, so errors.Is against the consumer's own sentinel keeps
// working through the wrap. A WIRE failure under the dispatch is a different
// thing and stays ErrWireFaceLost.
type EmitStopped struct {
	// Cause is the consumer's error, verbatim.
	Cause error
	// TxStatus is the session's transaction track after the target's tail was
	// drained — the same byte the loop's readiness would carry. 0 when it could
	// not be read.
	TxStatus byte
	// Outcome is the status RECORDED in the statement's outcome row: StatusOK,
	// StatusPendingCommit, StatusError, StatusRolledBack or StatusUnresolvable.
	Outcome string
	// Executed reports that a statement had been dispatched to the target
	// before the stop. False only for the EMPTY query, whose one frame
	// (EmptyQueryResponse) is not a statement: then Outcome is empty and there
	// are no effects to report — the loop says nothing about them.
	Executed bool
	// TargetErr is the target's error for that statement if its ErrorResponse
	// passed through the emitter (the frame that failed to emit, or one before
	// it). nil when no target error was observed — which is NOT "none occurred".
	TargetErr *pgconn.PgError
}

func (s *EmitStopped) Error() string {
	if !s.Executed {
		return fmt.Sprintf("exec: output stopped by the consumer; no statement ran (tx %s): %v",
			txStatusWord(s.TxStatus), s.Cause)
	}
	return fmt.Sprintf("exec: output stopped by the consumer after dispatch (outcome %s, tx %s): %v",
		s.Outcome, txStatusWord(s.TxStatus), s.Cause)
}

// EmitArm names what became of the statement whose output was cut.
type EmitArm string

const (
	// ArmNoStatement: nothing ran (the empty query); there are no effects.
	ArmNoStatement EmitArm = "no_statement"
	// ArmNotExecuted: the statement EXISTED but never ran, because an earlier
	// statement in the same buffer or segment failed and the target discarded
	// the rest. Distinct from ArmNoStatement: there, the client sent nothing;
	// here it sent a real statement that the target threw away. Telling the
	// second story with the first's words ("the query was empty") makes a client
	// conclude it never sent anything.
	ArmNotExecuted EmitArm = "not_executed"
	// ArmFailed: the target refused the statement; the error was observed.
	ArmFailed EmitArm = "failed"
	// ArmPending: executed inside the client's open transaction.
	ArmPending EmitArm = "pending_commit"
	// ArmAborted: executed; the transaction is aborted, effects will roll back.
	ArmAborted EmitArm = "aborted"
	// ArmCompleted: the engine observed the statement complete.
	ArmCompleted EmitArm = "completed"
	// ArmUnresolved: the tail was not observed; the outcome is not known.
	ArmUnresolved EmitArm = "unresolvable"
)

// Arm applies the six arms in their documented order. ONE implementation: the
// loop's wording and the audit's status both derive from this answer.
func (s *EmitStopped) Arm() EmitArm {
	switch {
	case !s.Executed && s.Outcome == "":
		// THE EMPTY QUERY, and only it. The two not-run cases already differ in
		// the struct: the empty query carries no outcome at all, while a
		// statement discarded by an earlier failure carries the outcome that was
		// RECORDED for it (StatusError / ErrNotExecuted). Reading only Executed
		// collapsed them, and the front door then told a client with a real
		// statement that its query was empty.
		return ArmNoStatement
	case !s.Executed:
		return ArmNotExecuted
	case s.TargetErr != nil:
		return ArmFailed
	case s.TxStatus == TxStatusInTx:
		return ArmPending
	case s.TxStatus == TxStatusAborted:
		return ArmAborted
	case s.Outcome == StatusOK:
		return ArmCompleted
	default:
		return ArmUnresolved
	}
}

// Unwrap exposes the consumer's own error to errors.Is / errors.As.
func (s *EmitStopped) Unwrap() error { return s.Cause }

// Unresolved reports the sixth arm: the outcome is not known to the front door.
func (s *EmitStopped) Unresolved() bool { return s.Arm() == ArmUnresolved }

func txStatusWord(b byte) string {
	switch b {
	case TxStatusIdle:
		return "idle"
	case TxStatusInTx:
		return "open"
	case TxStatusAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

// emitStopped builds the EmitStopped for a statement whose frames were cut,
// reading the session's transaction track the way the loop's readiness would.
func (e *Engine) emitStopped(s *session, cause error, outcome string, executed bool, targetErr *pgconn.PgError) error {
	st, _ := s.wireTxStatus()
	return &EmitStopped{Cause: cause, TxStatus: st, Outcome: outcome, Executed: executed, TargetErr: targetErr}
}

// emitStoppedWithStatus is emitStopped for a producer that KNOWS whether the
// tail was observed.
//
// TxStatus is authoritative only when it was. The raw producer gets that for
// free — golib drains to ReadyForQuery even on a consumer error, so the status
// it reads afterwards is post-tail — but a producer that stops reading early
// holds a snapshot from before the target had finished deciding. Pass 0 there:
// Arm() then falls through to ArmUnresolved instead of letting a stale
// in-transaction byte outrank an unresolved outcome and promise a client that
// effects are still pending when the target has already aborted them.
func (e *Engine) emitStoppedWithStatus(cause error, outcome string, executed bool,
	targetErr *pgconn.PgError, status byte) error {

	return &EmitStopped{Cause: cause, TxStatus: status, Outcome: outcome, Executed: executed, TargetErr: targetErr}
}
