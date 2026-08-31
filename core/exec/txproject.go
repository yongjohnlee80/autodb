package exec

import (
	"context"
	"errors"

	"github.com/yongjohnlee80/golib/dao"

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
	if !e.projectable(txID, state) {
		return
	}
	err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		return e.projectHistoryTx(tx, txID, state)
	})
	if err != nil {
		e.logf("projecting the %s outcome of %s onto history failed: %v", state, txID, err)
	}
}

// projectHistoryTx is the projection itself, inside a caller's transaction.
//
// Written to be callable from the same transaction as the terminal INSERT, so
// the two land together or not at all (PR #20 r0 MF3): a crash between them
// used to leave the truth terminal and the surface pending, forever, because
// reconciliation skips groups that already have a terminal.
func (e *Engine) projectHistoryTx(tx *dao.Transaction, txID string, state meta.TxState) error {
	if !e.projectable(txID, state) {
		return nil
	}
	err := e.store.History.On(tx).
		With(meta.HistTxID, txID).
		With(meta.HistStatus, StatusPendingCommit).
		Set(meta.HistStatus, historyStatusFor(state)).
		Update()
	if errors.Is(err, dao.ErrNoRows) {
		// Nothing was pending. The ordinary case for a transaction that ran
		// no statements, and for a repair pass over an already-projected
		// group — both are success, not failure.
		return nil
	}
	return err
}

// projectable reports whether this outcome has a history projection at all.
func (e *Engine) projectable(txID string, state meta.TxState) bool {
	// Not gated on e.history for correctness — it is gated because with
	// history off there is no surface to project ONTO. The outcome log still
	// holds the truth, which is why it is the thing that is never gated.
	return e.history && txID != "" && historyStatusFor(state) != ""
}

// repairHistory re-runs the projection for a settled transaction.
//
// Belt to the atomic write's braces. The atomicity closes the crash window
// for transitions written from here on; this heals a row left behind by a
// build that predates it, or by a projection that was skipped because history
// was disabled at the time and has since been turned back on. Idempotent: it
// touches only rows still marked pending.
func (e *Engine) repairHistory(ctx context.Context, txID string, state meta.TxState) {
	if !e.projectable(txID, state) {
		return
	}
	n, err := e.store.History.OnCtx(ctx).
		With(meta.HistTxID, txID).With(meta.HistStatus, StatusPendingCommit).Count()
	if err != nil || n == 0 {
		return
	}
	e.logf("repairing %d history row(s) left pending under settled transaction %s", n, txID)
	e.resolveHistory(ctx, txID, state)
}
