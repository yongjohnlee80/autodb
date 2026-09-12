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
// one from a transaction status byte (`I` is left behind by a
// committed autocommit AND by a failed one; the status alone proves nothing).
//
// The loop obtains it with errors.As and asks Arm() what became of the
// statement. Arm encodes the six arms and their ORDER once, here, so the loop
// and the audit cannot disagree about them:
//
//  1. !Executed — no statement ran (the empty query): there are NO effects to
//     describe, whatever the transaction status says — an empty query inside
//     BEGIN leaves T and still has nothing pending. Checked before every
//     effect arm for exactly that reason.
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
	// Delivery marks this as a DELIVERY-scoped report rather than a statement's.
	//
	// EmitStopped models one statement's effects: every field below and every
	// arm above is a statement fact. A segment-ending drive has no statement to
	// describe -- Sync owns delivery, and the statements a segment covers are
	// settled by their own drives -- so a report from one must not be read
	// through that model.
	//
	// This is a DISCRIMINATOR, not a hint: Arm() returns ArmDeliveryStopped for
	// it and never a statement arm, so no caller can accidentally derive
	// "the statement ran" from a report that never described a statement.
	// Executed, Outcome and TargetErr are left zero in this scope, because the
	// drive genuinely does not know them; coercing wire traffic into Executed
	// is what made this necessary.
	Delivery bool

	// Cause is the consumer's error, verbatim.
	Cause error
	// TxStatus is the session's transaction track after the target's tail was
	// drained — the same byte the loop's readiness would carry. 0 when it could
	// not be read.
	TxStatus byte
	// Outcome is the status RECORDED in the statement's outcome row: StatusOK,
	// StatusPendingCommit, StatusError, StatusRolledBack or StatusUnresolvable.
	Outcome HistStatus
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

// Error formats a descriptive message describing why execution or delivery was stopped.
// EmitStopped implements the error interface.
func (s *EmitStopped) Error() string {
	if s.Delivery {
		return fmt.Sprintf("exec: the segment's answers were not delivered (tx %s): %v",
			txStatusWord(s.TxStatus), s.Cause)
	}
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

	// ArmDeliveryStopped: the SEGMENT's answers were not delivered, and this
	// report says nothing about any statement.
	//
	// It is not a seventh statement outcome. The other six answer "what became
	// of this statement's effects"; this one answers a different question, and
	// the distinction is the whole reason it exists. A Sync owns segment
	// DELIVERY: the segment may carry no statement at all -- Parse/Describe/Sync
	// is what pgx's default mode and database/sql's Prepare send -- or several,
	// each already settled by its own Execute drive.
	//
	// Before it, a delivery stop borrowed statement vocabulary and a segment
	// with NO STATEMENT was told "the statement ran and its outcome is not
	// known to the front door ... read the table to find out". An operator sent
	// to inspect a table for effects that never existed.
	ArmDeliveryStopped EmitArm = "delivery_stopped"
)

// Arm applies the six arms in their documented order. ONE implementation: the
// loop's wording and the audit's status both derive from this answer.
func (s *EmitStopped) Arm() EmitArm {
	switch {
	case s.Delivery:
		// FIRST, so no statement arm below can be reached by a report that
		// never described a statement. The order is the guarantee: a delivery
		// report with a T status must not become ArmPending and tell a client
		// its non-existent statement's effects are pending inside a
		// transaction.
		return ArmDeliveryStopped
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

// txStatusWord converts a wire transaction status byte into a human-readable string.
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
func (e *Engine) emitStopped(s *session, cause error, outcome HistStatus, executed bool, targetErr *pgconn.PgError) error {
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
func (e *Engine) emitStoppedWithStatus(cause error, outcome HistStatus, executed bool,
	targetErr *pgconn.PgError, status byte) error {

	return &EmitStopped{Cause: cause, TxStatus: status, Outcome: outcome, Executed: executed, TargetErr: targetErr}
}

// deliveryStopped builds the DELIVERY-scoped report for a segment whose
// answers were cut short by the consumer.
//
// Separate constructor rather than a flag on emitStoppedWithStatus, so a caller
// cannot half-fill one: the statement fields are not merely left zero here,
// there is no way to pass them. The status is the caller's because only a
// segment-ending drive has a truthful one — see WireSyncSegment.
func (e *Engine) deliveryStopped(cause error, status byte) error {
	return &EmitStopped{Delivery: true, Cause: cause, TxStatus: status}
}
