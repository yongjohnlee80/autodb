package exec

import (
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The terminal write: every attempt reaches ONE disposition.
//
// A terminal is written by a compare-and-set on the attempt's own row — keyed
// by the history row id AND the attempt id, conditioned on the disposition
// still being unset — never by an unconditional UPDATE. So two writers, or one
// retried after a transient error, cannot produce two terminals or overwrite
// the first. When the compare-and-set changes nothing, the row that is there
// decides:
//
//   - the same disposition: an identical repeat, accepted as a no-op. Judged
//     on the disposition alone, never on status: a transaction's commit
//     refines status after the first write, and the retry is still the same
//     delivery it was before.
//   - a different one: ErrDispositionConflict, and a disposition_conflict
//     audit row naming both. Nothing is overwritten.
//   - no row with that id and attempt: ErrNoAttempt, a writer bug.
//
// The promise holds with history ON, where the row exists to condition on.
// With history off there is no durable per-attempt record, so there is no
// compare-and-set and no duplicate prevention; the audit row still carries
// the attempt id for correlation, and nothing claims more.

// ErrDispositionConflict reports a terminal for an attempt that already has a
// different one. The first stands.
var ErrDispositionConflict = errors.New("exec: the attempt already has a different disposition")

// ErrNoAttempt reports a terminal for an attempt with no history row: a writer
// handed an Attempt it did not get from recordAttemptTagged.
var ErrNoAttempt = errors.New("exec: no history row for this attempt")

// dispositionFor is what an attempt did, from its status AND whether its
// completion was observed.
//
// The disposition is about the ATTEMPT and status is about the EFFECT, so status
// alone cannot decide it. outcome_unresolvable says the effect's fate is
// unknown, and in an implicit block cut by a client disconnect that is true of
// every statement — including the ones whose CommandComplete the engine saw.
// Those completed; only the statements whose completion was never observed are
// unknown, the same state of knowledge as a process that died before recording
// its outcome.
func dispositionFor(status HistStatus, completionObserved bool) (meta.Disposition, error) {
	switch status {
	case StatusOK, StatusPendingCommit:
		return meta.DispositionCompleted, nil
	case StatusError:
		return meta.DispositionFailed, nil
	case StatusRolledBack:
		return meta.DispositionRolledBack, nil
	case StatusUnresolvable:
		if completionObserved {
			return meta.DispositionCompleted, nil
		}
		return meta.DispositionUnknown, nil
	case StatusUnknown:
		return meta.DispositionUnknown, nil
	}
	return "", fmt.Errorf("exec: %q is not a terminal status", status)
}

// settleTx makes att terminal inside tx: the compare-and-set, then the
// duplicate-delivery rules above. It reports whether THIS call made the attempt
// terminal; the caller writes the terminal's audit row only then, so an
// identical repeat adds no second exec_result.
//
// A conflict is RECORDED here, in tx, and reported as ErrDispositionConflict:
// the caller must still commit tx, or the disposition_conflict row goes with
// the rollback.
//
// userID and ip are the conflict row's: the caller's, or — for the recovery of
// a dead owner's attempt, which has no caller — the attempt's own.
func (e *Engine) settleTx(tx *dao.Transaction, userID int64, ip string, connID int64,
	att Attempt, disp meta.Disposition, set map[meta.HistoryField]any) (bool, error) {
	q := e.store.History.On(tx).
		With(meta.HistID, att.HistID).With(meta.HistAttemptID, att.ID).
		With(meta.HistDisposition, "").
		Set(meta.HistDisposition, string(disp))
	for f, v := range set {
		q = q.Set(f, v)
	}
	// COUNTED, because the condition is the whole point: Update says nothing
	// about whether its WHERE matched, and a compare-and-set that cannot tell
	// it lost is an unconditional write.
	n, err := dao.UpdateAffected(q)
	if err != nil {
		return false, fmt.Errorf("exec: completing history: %w", err)
	}
	switch n {
	case 1:
		return true, nil
	case 0:
	default:
		return false, fmt.Errorf("exec: completing history: one attempt's write changed %d rows", n)
	}
	row, gerr := e.store.History.On(tx).
		With(meta.HistID, att.HistID).With(meta.HistAttemptID, att.ID).Get()
	if errors.Is(gerr, dao.ErrNoRows) {
		return false, fmt.Errorf("%w: attempt %s, history row %d", ErrNoAttempt, att.ID, att.HistID)
	}
	if gerr != nil {
		return false, fmt.Errorf("exec: reading a settled attempt: %w", gerr)
	}
	if row.Disposition == disp {
		return false, nil // an identical repeat: the first write stands
	}
	if aerr := e.auth.AuditTxRecord(tx, auth.AuditRecord{UserID: userID, IP: ip,
		Action: "disposition_conflict",
		Detail: fmt.Sprintf("conn %d: attempt already %s, refused %s", connID, row.Disposition, disp),
		TxID:   row.TxID, AttemptID: att.ID, ConnID: connID}); aerr != nil {
		return false, aerr
	}
	return false, fmt.Errorf("%w: attempt %s is %s, not %s", ErrDispositionConflict, att.ID, row.Disposition, disp)
}
