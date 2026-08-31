package exec

import (
	"context"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Outcome-log retention — ADR-0079 §3, phase P4.
//
// Retention here COLLAPSES a settled transaction's progression to its terminal
// and stops. It never deletes the transaction from the log, and that is the
// whole mechanism rather than an implementation detail.
//
// The reason is that absence is load-bearing. `ErrNoSuchTx` means "no
// transaction was started", and its truthfulness rests on the write-ahead
// ordering in beginTx: the opened row is durable BEFORE the target is asked to
// begin anything, so zero rows PROVES nothing started. Delete a settled
// transaction and that proof becomes a lie — a committed transaction begins
// answering "no such transaction", which is the same failure the write-ahead
// ordering was introduced to prevent (ADR-0074 Amendment 5 decision 5).
//
// It is also why time-range partitioning cannot be the retention mechanism for
// this table: ADR-0079 measured that partitioning by `created_at` forces the
// partition key into every unique index and thereby destroys both durable
// guards. Detaching a partition is deletion with extra steps.
//
// What it buys: a settled transaction costs one row instead of four or five.
// That is the real growth term, since the progression is bounded but the
// number of transactions is not.

// CollapseSettledOutcomes prunes the intermediate transitions of settled
// transactions older than `before`, leaving each terminal as a tombstone.
//
// Returns the number of transactions collapsed. Safe to call repeatedly: a
// transaction whose progression is already a lone terminal has nothing to
// prune and is skipped, so a second pass over the same window is a no-op
// rather than a rewrite.
func (e *Engine) CollapseSettledOutcomes(ctx context.Context, before time.Time) (int, error) {
	cutoff := before.Unix()
	collapsed := 0

	// The keyset paging and the tombstone EXCLUSION both live in meta.Sweep
	// now. Excluding already-collapsed rows AT THE QUERY is load-bearing (PR
	// #22 r0 MF1): a tombstone still satisfies created_at < cutoff, so
	// filtering it in the loop instead fills pages with rows on which no
	// progress is made and starves every eligible progression behind them.
	// This function was the third consumer in one arc to hand-assemble that
	// shape wrong, which is why the shape is a component.
	_, err := meta.Sweep(ctx, e.store.TxOutcomes,
		meta.SweepSpec[*meta.TxOutcome, meta.TxOutcomeField, meta.Sort]{
			Key:      meta.TxOutID,
			ByKey:    meta.TxOutByID,
			KeyOf:    func(r *meta.TxOutcome) int64 { return r.ID },
			PageSize: maxReconcileBatch,
			Where: []dao.Predicate{
				dao.Lt(string(meta.TxOutCreatedAt), cutoff),
				dao.Eq(string(meta.TxOutCollapsedAt), int64(0)),
			},
		},
		func(page []*meta.TxOutcome) error {
			byTx := map[string][]*meta.TxOutcome{}
			for _, r := range page {
				byTx[r.TxID] = append(byTx[r.TxID], r)
			}
			n, err := e.collapseGroups(ctx, byTx, cutoff)
			collapsed += n
			return err
		})
	return collapsed, err
}

// collapseGroups collapses whichever of these transactions are eligible.
func (e *Engine) collapseGroups(ctx context.Context, byTx map[string][]*meta.TxOutcome, cutoff int64) (int, error) {
	collapsed := 0
	for txID, group := range byTx {
		// Only SETTLED transactions. An unresolved one still needs its
		// progression — the reconciler reads `commit_started` to know a
		// COMMIT was in flight, and the xid on it to ask the oracle. Pruning
		// that would destroy the recovery path this log exists for.
		st := foldTxLog(group)
		if !st.Terminal() {
			continue
		}
		// The whole progression must be inside the window. A transaction
		// that settled recently but opened long ago would otherwise have its
		// opening pruned while it is still of interest.
		full, err := e.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, txID).Select()
		if err != nil {
			return collapsed, fmt.Errorf("exec: reading %s for retention: %w", txID, err)
		}
		if len(full) < 2 {
			continue // already a lone terminal: nothing to collapse
		}
		newest := int64(0)
		for _, r := range full {
			if r.CreatedAt > newest {
				newest = r.CreatedAt
			}
		}
		if newest >= cutoff {
			continue
		}
		if err := e.collapseOne(ctx, txID, full); err != nil {
			return collapsed, err
		}
		collapsed++
	}
	return collapsed, nil
}

// collapseOne removes a settled transaction's non-terminal transitions and
// stamps the surviving terminal as a tombstone, in ONE store transaction.
//
// Atomic for the same reason the terminal and its history projection are: a
// crash between the delete and the stamp would leave a progression that has
// lost its transitions and does not say so, which is indistinguishable from a
// transaction that only ever had a terminal.
func (e *Engine) collapseOne(ctx context.Context, txID string, group []*meta.TxOutcome) error {
	var terminal *meta.TxOutcome
	for _, r := range group {
		if meta.TxState(r.State).IsTerminal() {
			terminal = r
			break
		}
	}
	if terminal == nil {
		return nil
	}
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		for _, r := range group {
			if r.ID == terminal.ID {
				continue
			}
			if err := e.store.TxOutcomes.On(tx).With(meta.TxOutID, r.ID).Delete(); err != nil {
				return fmt.Errorf("exec: pruning transition %d of %s: %w", r.Seq, txID, err)
			}
		}
		// The stamp is what makes a tombstone legible as one. Without it a
		// collapsed progression is indistinguishable from a short one, and a
		// reader cannot tell that transitions were dropped.
		return e.store.TxOutcomes.On(tx).With(meta.TxOutID, terminal.ID).
			Set(meta.TxOutCollapsedAt, e.now().Unix()).Update()
	})
}

// StartOutcomeRetention runs retention on a ticker, if it is enabled.
//
// Disabled by default and by design. Nothing in autodb needs this until the
// outcome log is large enough to be a problem, and ADR-0079 §3 records that at
// one row per transition it is orders of magnitude below the volume tables. It
// exists now so that when an operator does turn it on, the invariant holds by
// construction rather than by whoever implements it later remembering to.
//
// A non-positive interval or retention period disables it — the same named
// semantics as reconcile_interval (ADR-0074 Amendment 4 A1).
func (e *Engine) StartOutcomeRetention(ctx context.Context, every, keep time.Duration) {
	if every <= 0 || keep <= 0 {
		return
	}
	e.bgWG.Add(1)
	go func() {
		defer e.bgWG.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-e.bgCtx.Done():
				return
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := e.CollapseSettledOutcomes(ctx, e.now().Add(-keep))
				if err != nil {
					e.logf("outcome retention: %v", err)
					continue
				}
				if n > 0 {
					e.logf("outcome retention collapsed %d settled transaction(s) to tombstones", n)
				}
			}
		}
	}()
}
