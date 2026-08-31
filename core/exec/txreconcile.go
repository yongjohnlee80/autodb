package exec

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Recovery reconciliation — ADR-0074 §7 rev 2 + Amendment 4.
//
// The outcome log records that a COMMIT was in flight; it cannot record what
// happened to a COMMIT the process did not survive. That is what the target's
// own transaction status is for: after a crash the engine has no connection
// state left, but PostgreSQL still knows whether xid N committed or aborted,
// and txid_status(bigint) will say so.
//
// The reconciler is what turns a durable "we do not know" into a true
// outcome. Everything it does is governed by one rule: it may only ever
// APPEND a terminal it has PROVEN. When it cannot prove one it leaves the
// entry pending and visible, and the entry is retried — an unresolved outcome
// is never silently dropped, and never guessed at.
//
// Which entries need it:
//
//   - commit_started with no terminal — the real crash window. The COMMIT may
//     or may not have landed, and only the oracle can say.
//   - unknown_pending — either a live indeterminate commit
//     (ErrTxOutcomeUnknown, no crash required) or one of R3's deferred
//     rollback paths.
//
// Which do NOT:
//
//   - opened with no terminal. A transaction that never reached
//     commit_started cannot have committed: the server aborts it when the
//     connection dies. That needs no oracle, and giving it one would be
//     asking a question whose answer is already known.

// txReconcileBackoff is the per-entry retry floor. An entry whose target is
// unreachable would otherwise be re-probed on every pass, turning a down
// database into a tight loop of connection attempts against it.
const txReconcileBackoff = 30 * time.Second

// reconciler holds the cross-pass state: which entries are in flight right
// now, and when each may next be attempted.
type reconciler struct {
	mu       sync.Mutex
	inFlight map[string]bool
	nextTry  map[string]time.Time
}

func newReconciler() *reconciler {
	return &reconciler{inFlight: map[string]bool{}, nextTry: map[string]time.Time{}}
}

// claim admits one resolver per tx_id per process.
//
// The instance lease gives one PROCESS, not one goroutine (lector Amendment-4
// MF3), and inside this process the periodic pass, a checkout trigger and the
// boundary handler can all reach the same transaction. The store's terminal
// guard is what makes a lost race SAFE; this is what stops us from running
// the race — and paying for two txid_status round-trips — in the first place.
func (r *reconciler) claim(txID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[txID] {
		return false
	}
	if next, ok := r.nextTry[txID]; ok && now.Before(next) {
		return false
	}
	r.inFlight[txID] = true
	return true
}

// release ends a claim. A resolved entry is forgotten entirely rather than
// left with a backoff, because it will never be seen again.
func (r *reconciler) release(txID string, now time.Time, resolved bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, txID)
	if resolved {
		delete(r.nextTry, txID)
		return
	}
	r.nextTry[txID] = now.Add(txReconcileBackoff)
}

// claimed reports whether a backoff is currently recorded for a transaction.
// Test seam: the backoff is otherwise only observable by waiting for it.
func (r *reconciler) claimed(txID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.nextTry[txID]
	return ok
}

// ReconcileOutcomes resolves what it can and reports how many it settled.
//
// Safe to call repeatedly and safe to call concurrently with the boundary
// handler: every terminal goes through the same appender, so a losing writer
// defers to the outcome that landed instead of contradicting it. That is what
// makes restart idempotence hold — a second run over an already-resolved log
// finds nothing to do.
func (e *Engine) ReconcileOutcomes(ctx context.Context) int {
	rows, err := e.store.TxOutcomes.OnCtx(ctx).Select()
	if err != nil {
		e.logf("reconciling outcomes: reading the log: %v", err)
		return 0
	}
	byTx := map[string][]*meta.TxOutcome{}
	for _, r := range rows {
		byTx[r.TxID] = append(byTx[r.TxID], r)
	}

	now := e.now()
	resolved := 0
	for txID, group := range byTx {
		st := foldTxLog(group)
		if st.Terminal() || !needsOracle(st.State) {
			continue
		}
		if !e.reconcile.claim(txID, now) {
			continue
		}
		done := e.resolveOne(ctx, txID, st, group)
		e.reconcile.release(txID, now, done)
		if done {
			resolved++
		}
	}
	return resolved
}

// needsOracle reports whether a nonterminal state can only be settled by
// asking the target.
//
// `opened` is deliberately excluded: a transaction that never reached
// commit_started cannot have committed. It is left alone here and settled by
// the paths that own it — the boundary handler, the timeout sweep, the
// janitor — rather than being terminated by a reconciler that has no idea
// whether the session is still live and about to commit it.
func needsOracle(s meta.TxState) bool {
	return s == meta.TxCommitStarted || s == meta.TxUnknownPending
}

// resolveOne settles one transaction, and reports whether it managed to.
func (e *Engine) resolveOne(ctx context.Context, txID string, st TxStatus, group []*meta.TxOutcome) bool {
	xid := ""
	for _, r := range group {
		if r.TargetXID != "" {
			xid = r.TargetXID
		}
	}

	connRow, err := e.connectionRow(ctx, st.ConnID)
	if err != nil {
		// The connection is gone, so no oracle can ever be consulted for this
		// entry again. That is a permanent condition, not a transient one:
		// terminating it honestly is better than retrying forever against a
		// target that no longer exists.
		return e.terminate(ctx, txID, st, meta.TxUnresolvable, meta.ReasonNoOracle)
	}

	// No oracle on this dialect. MySQL and sqlite have nothing equivalent to
	// txid_status, so an indeterminate commit there can never be resolved by
	// anyone — which is a terminal condition, and Amendment 4 MF2 makes it
	// one by OUTCOME rather than by cause.
	if connRow.Engine != "postgres" || xid == "" {
		return e.terminate(ctx, txID, st, meta.TxUnresolvable, meta.ReasonNoOracle)
	}

	status, err := e.txidStatus(ctx, connRow, xid)
	if err != nil {
		// Unreachable, or credentials have drifted. Transient by assumption,
		// so the entry stays pending and VISIBLE and is retried after the
		// backoff. This is the one branch that must not terminate: guessing
		// here is how a committed transaction gets recorded as rolled back.
		e.logf("reconciling %s: asking the target about xid %s: %v", txID, xid, err)
		return false
	}

	switch status {
	case "committed":
		return e.terminate(ctx, txID, st, meta.TxCommitted, "")
	case "aborted":
		return e.terminate(ctx, txID, st, meta.TxRolledBack, "")
	case "in progress":
		// Still running, so there is nothing to record yet. Not an error and
		// not a resolution — the next pass asks again.
		return false
	}
	// A NULL answer means the status data has been discarded: the xid has
	// passed out of the server's retention horizon and the truth is now
	// unknowable BY ANYONE. Terminal, and named as such, so it stops being
	// retried and an operator can see why it will never resolve.
	return e.terminate(ctx, txID, st, meta.TxUnresolvable, meta.ReasonXIDHorizon)
}

// terminate appends a proven terminal and reports success.
func (e *Engine) terminate(ctx context.Context, txID string, st TxStatus, state meta.TxState, reason string) bool {
	if err := e.appendTxOutcome(ctx, txTransition{
		txID: txID, state: state, reason: reason,
		userID: st.UserID, connectionID: st.ConnID,
	}); err != nil {
		e.logf("reconciling %s: recording %s: %v", txID, state, err)
		return false
	}
	return true
}

// txidStatus asks the target what became of a transaction id.
//
// Returns the server's own word — "committed", "aborted", "in progress" — or
// "" for the NULL that means the status data has aged out. The distinction
// between "" and an error is load-bearing: "" is a permanent answer and an
// error is a transient failure, and they resolve to opposite actions.
func (e *Engine) txidStatus(ctx context.Context, connRow *meta.Connection, xid string) (string, error) {
	target, err := e.target(ctx, connRow.ID, connRow)
	if err != nil {
		return "", err
	}
	qctx, cancel := context.WithTimeout(ctx, txCleanupTimeout)
	defer cancel()

	// COALESCE so a NULL arrives as a value this code can read, rather than
	// as a scan into a *string that would have to be nil-checked separately.
	rows, err := target.QueryContext(qctx,
		"SELECT COALESCE(txid_status($1::text::bigint), '')", xid)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", fmt.Errorf("exec: txid_status returned no row for %s", xid)
	}
	var status string
	if err := rows.Scan(&status); err != nil {
		return "", err
	}
	return status, nil
}

// connectionRow loads a connection, or reports that it is gone.
func (e *Engine) connectionRow(ctx context.Context, connID int64) (*meta.Connection, error) {
	row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		return nil, fmt.Errorf("exec: connection %d: %w", connID, err)
	}
	return row, nil
}

// StartOutcomeReconciler runs the reconciler at startup and then on a ticker.
//
// Both are required (ADR-0074 §7): the startup pass is what recovers the
// crash window — it is the whole reason the log exists — and the periodic
// pass is what resolves entries whose target was down when the startup pass
// ran, and the live indeterminate commits that need no crash at all.
func (e *Engine) StartOutcomeReconciler(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	go func() {
		// The startup pass runs first and unconditionally. Waiting a full
		// tick would leave every crash-window transaction unresolved for that
		// long, which is precisely the interval an operator is staring at the
		// pending list.
		if n := e.ReconcileOutcomes(ctx); n > 0 {
			e.logf("startup reconciliation resolved %d transaction outcome(s)", n)
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.ReconcileOutcomes(ctx)
			}
		}
	}()
}
