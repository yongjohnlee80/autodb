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

// dequeueSettled clears a queue entry whose transaction is already settled.
//
// Reachable two ways: a terminal written by a build that predates the queue,
// and a process that died between the terminal and its own dequeue — though
// the latter is now impossible for new writes, since both share one store
// transaction. Clearing it keeps the backlog equal to the real backlog.
func (e *Engine) dequeueSettled(ctx context.Context, txID string, st TxStatus) {
	err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if derr := e.dequeuePendingTx(tx, txID); derr != nil {
			return derr
		}
		// Its surface may still be pending for the same reason the entry
		// was, so project while we are here.
		return e.projectHistoryTx(tx, txID, st.State)
	})
	if err != nil {
		e.logf("clearing the settled queue entry for %s: %v", txID, err)
	}
}

// repairPendingHistory heals history rows stranded under a settled outcome.
//
// Driven from HISTORY rather than from the outcome log, which is the whole
// point: the strandable rows are exactly the ones still marked
// ok_pending_commit, and there are normally none. Walking settled
// TRANSACTIONS to find them meant one history query per transaction ever
// recorded, on every pass (PR #20 r1 MF1) — a scan whose cost grew with
// history while the thing it looked for did not.
//
// Bounded, and re-run each pass, so a backlog larger than one batch drains
// over several passes instead of being materialized at once.
func (e *Engine) repairPendingHistory(ctx context.Context) {
	if !e.history {
		return
	}
	// Resume where the last sweep stopped, for the same reason the
	// reconciler does (PR #20 r2 MF2). A fixed first page is fatal here in
	// exactly the same way: a screenful of legitimately in-flight pending
	// rows sits at the front, this sweep correctly leaves them alone, and a
	// genuinely stranded row behind them is never reached on any pass.
	//
	// The cursor is on history id, and it wraps on a short page so rows
	// ahead of it come round again.
	after := e.reconcile.cursor(repairCursorScope)
	q := e.store.History.OnCtx(ctx).
		With(meta.HistStatus, StatusPendingCommit).
		OrderBy(dao.Asc(meta.HistByID))
	if after > 0 {
		q = q.WithPredicate(dao.Gt(string(meta.HistID), after))
	}
	rows, err := q.Limit(maxReconcileBatch).Select()
	if err != nil {
		e.logf("looking for stranded history rows: %v", err)
		return
	}
	next := int64(0)
	if len(rows) == maxReconcileBatch {
		for _, r := range rows {
			if r.ID > next {
				next = r.ID
			}
		}
	}
	e.reconcile.setCursor(repairCursorScope, next)

	seen := map[string]bool{}
	for _, r := range rows {
		if r.TxID == "" || seen[r.TxID] {
			continue
		}
		seen[r.TxID] = true
		// Only the log can say whether this row is stranded or simply still
		// in flight, and only the settled ones are ours to touch.
		group, gerr := e.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, r.TxID).Select()
		if gerr != nil || len(group) == 0 {
			continue
		}
		st := foldTxLog(group)
		if !st.Terminal() {
			continue
		}
		e.logf("repairing history left pending under settled transaction %s", r.TxID)
		e.resolveHistory(ctx, r.TxID, st.State)
	}
}

// repairCursorScope keys the repair sweep's paging position in the shared
// cursor map. Negative so it can never collide with a connection id, which
// keys the reconciler's own scopes.
const repairCursorScope int64 = -1

// --- the pending queue ------------------------------------------------------

// enqueuePendingTx records that this transaction has no terminal yet.
//
// A duplicate is success: the append path can retry after a seq collision,
// and the entry from the first attempt is exactly what we would write.
func (e *Engine) enqueuePendingTx(tx *dao.Transaction, txID string, connID, userID int64) error {
	_, err := e.store.TxPending.On(tx).
		Set(meta.TxPendTxID, txID).
		Set(meta.TxPendConnID, connID).
		Set(meta.TxPendUserID, userID).
		Set(meta.TxPendCreatedAt, e.now().Unix()).
		Insert()
	if errors.Is(err, dao.ErrDuplicate) {
		return nil
	}
	return err
}

// dequeuePendingTx removes a settled transaction from the queue.
//
// An absent row is success. A transaction settled by a build that predates
// the queue has nothing to remove, and failing a terminal write over
// bookkeeping already in the desired state would be the wrong trade.
func (e *Engine) dequeuePendingTx(tx *dao.Transaction, txID string) error {
	err := e.store.TxPending.On(tx).With(meta.TxPendTxID, txID).Delete()
	if errors.Is(err, dao.ErrNoRows) {
		return nil
	}
	return err
}
