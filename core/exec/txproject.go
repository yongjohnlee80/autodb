package exec

import (
	"context"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// History as a PROJECTION of the outcome log — ADR-0074 §7 rev 2.
//
// script_history.status used to be authored directly: a statement that did
// not error was written "ok" and that was the end of it. Inside a transaction
// that is a claim the engine cannot support, because whether the effect
// survives is decided later — by the COMMIT, and after a crash possibly by a
// different process entirely.
//
// So the status of an in-transaction statement is now derived. The outcome
// log is the truth; history holds a materialized view of it, refreshed when
// the transaction terminates. The refresh is idempotent and never authors an
// outcome of its own — every status it writes is read out of the log — which
// is what keeps "history is a projection" true rather than aspirational.

// Statement status vocabulary.
//
// Named constants because these strings are written by the engine, read by
// the projection, and rendered by three consumers; a typo in any one of them
// is a status nothing matches and no test would notice.
const (
	StatusRunning = "running"
	// StatusOK means the effect is durable. Outside a transaction that is
	// true as soon as the statement returns; inside one it is true only
	// after the commit, which is why it is not written there.
	StatusOK = "ok"
	// StatusPendingCommit means the statement ran and its fate is the
	// transaction's. This is the honest answer for in-transaction work, and
	// it is the one the old code could not express.
	StatusPendingCommit = "ok_pending_commit"
	StatusError         = "error"
	// StatusRolledBack means the statement ran and was then discarded.
	StatusRolledBack = "rolled_back"
	// StatusUnresolvable means nothing can ever say whether the effect
	// survived. Distinct from error — the statement did not fail — and
	// distinct from pending, because no future pass will improve on it.
	StatusUnresolvable = "outcome_unresolvable"
)

// historyStatusFor maps a transaction's terminal onto its statements' status.
func historyStatusFor(state meta.TxState) string {
	switch state {
	case meta.TxCommitted:
		return StatusOK
	case meta.TxRolledBack:
		return StatusRolledBack
	case meta.TxUnresolvable:
		return StatusUnresolvable
	}
	return ""
}

// resolveHistory refreshes the history rows belonging to a settled
// transaction.
//
// Only rows still marked pending are touched. That is what makes it safe to
// call from every resolver and safe to call twice: a row that already carries
// its terminal status is left exactly as it is, so a second pass changes
// nothing, and a statement that ERRORED keeps its own error rather than being
// overwritten by the transaction's outcome — the statement failing and the
// transaction rolling back are different facts, and the row that recorded the
// first should not be rewritten to claim only the second.
//
// A failure here is logged, not returned. History is a projection: losing a
// refresh costs a stale status on a row whose truth is still in the outcome
// log, and failing a caller's COMMIT over it would trade a real operation for
// a cosmetic one.
func (e *Engine) resolveHistory(ctx context.Context, txID string, state meta.TxState) {
	status := historyStatusFor(state)
	if status == "" || txID == "" {
		return
	}
	if !e.history {
		// Nothing to project onto. The outcome log still holds the truth,
		// which is the whole reason it is not gated on this setting.
		return
	}
	err := e.store.History.OnCtx(ctx).
		With(meta.HistTxID, txID).
		With(meta.HistStatus, StatusPendingCommit).
		Set(meta.HistStatus, status).
		Update()
	if err != nil {
		e.logf("projecting the %s outcome of %s onto history failed: %v", state, txID, err)
	}
}
