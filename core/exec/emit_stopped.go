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
// The loop tells the four arms apart with errors.As, in this order:
//
//   - TargetErr != nil — the target's ErrorResponse for that statement passed
//     through the emitter: it FAILED; in autocommit nothing was kept.
//   - TxStatus == 'T' — the client's transaction is still open: its effects are
//     PENDING; COMMIT or ROLLBACK still decides them (Outcome is
//     StatusPendingCommit).
//   - TxStatus == 'E' — the transaction is aborted: its effects will be rolled
//     back.
//   - Outcome == StatusOK — the engine observed the statement COMPLETE (its
//     CommandComplete arrived; the owned-control and decoded paths always have
//     the whole result before the first emit): the effects were kept.
//   - !Executed — the empty query: nothing ran, Outcome is "", no effects.
//   - Outcome == StatusUnresolvable — the tail was drained UNOBSERVED (golib
//     stops delivery at the first emit error and drains to ReadyForQuery
//     without it): whether the target kept the statement is not known to the
//     front door. "Unresolved", never a guess.
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
	return fmt.Sprintf("exec: output stopped by the consumer after dispatch (outcome %s, tx %s): %v",
		s.Outcome, txStatusWord(s.TxStatus), s.Cause)
}

// Unwrap exposes the consumer's own error to errors.Is / errors.As.
func (s *EmitStopped) Unwrap() error { return s.Cause }

// Unresolved reports the fourth arm: the outcome is not known to the front door.
func (s *EmitStopped) Unresolved() bool { return s.Outcome == StatusUnresolvable }

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
