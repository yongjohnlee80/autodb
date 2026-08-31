package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yongjohnlee80/golib/dao"

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
	// cursors carry each scope's paging position between passes, so a pass
	// resumes rather than restarting. Keyed by connection id, 0 for the
	// unscoped pass.
	cursors map[int64]int64
	// nextCheckout throttles the checkout trigger per connection, so the
	// engine's hottest path does not become a meta-store query per statement.
	nextCheckout map[int64]time.Time
}

func newReconciler() *reconciler {
	return &reconciler{
		inFlight:     map[string]bool{},
		nextTry:      map[string]time.Time{},
		cursors:      map[int64]int64{},
		nextCheckout: map[int64]time.Time{},
	}
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

// checkoutTrigger fires a scoped reconciliation after a connection is
// successfully used, at most once per connection per interval.
//
// Throttled because target() is the hottest path in the engine and this must
// not turn every statement into a meta-store query. The throttle is also why
// it runs in the BACKGROUND: a caller waiting for their own statement should
// never wait on the resolution of somebody else's abandoned transaction.
//
// It runs on the ENGINE's context, not the caller's and not a detached one.
// The caller's is cancelled the moment their statement finishes, which would
// abort the very work this exists to do; a detached one would outlive
// Engine.Close and could reopen a pool that had just been shut. The engine's
// is cancelled by Close, which then waits for this to finish before touching
// any pool.
func (e *Engine) checkoutTrigger(connID int64) {
	if !e.reconcile.claimCheckout(connID, e.now()) {
		return
	}
	e.bgWG.Add(1)
	go func() {
		defer e.bgWG.Done()
		ctx, cancel := context.WithTimeout(e.bgCtx, txCleanupTimeout)
		defer cancel()
		e.ReconcileConnection(ctx, connID)
	}()
}

// claimCheckout throttles the checkout trigger per connection.
func (r *reconciler) claimCheckout(connID int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if next, ok := r.nextCheckout[connID]; ok && now.Before(next) {
		return false
	}
	r.nextCheckout[connID] = now.Add(txReconcileBackoff)
	return true
}

func (r *reconciler) cursor(scope int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursors[scope]
}

func (r *reconciler) setCursor(scope, next int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if next == 0 {
		delete(r.cursors, scope)
		return
	}
	r.cursors[scope] = next
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
	return e.reconcileScope(ctx, 0)
}

// ReconcileConnection reconciles only the entries belonging to one connection.
//
// The checkout trigger (ADR-0074 Amendment 4 A1): a pending entry is retried
// on the next successful use of its own connection, which resolves promptly
// when a target comes back without paying for tighter polling. It is also
// what makes a disabled periodic pass coherent rather than a way to strand
// entries — with the ticker off, this is what still notices a recovered
// target while the daemon stays up.
func (e *Engine) ReconcileConnection(ctx context.Context, connID int64) int {
	return e.reconcileScope(ctx, connID)
}

// reconcileScope resolves the pending backlog, optionally narrowed to one
// connection.
func (e *Engine) reconcileScope(ctx context.Context, connID int64) int {
	// Start where the last pass stopped. A LIMIT with no cursor is a page,
	// not a position: with a screenful of LIVE `opened` rows at the front —
	// which this pass correctly skips — every pass re-reads them and a
	// resolvable entry behind them is never reached at all. That is
	// starvation, not slowness, and no amount of waiting fixes it
	// (PR #20 r2 MF1).
	byTx, next, err := e.pendingGroups(ctx, connID, maxReconcileBatch, e.reconcile.cursor(connID))
	if err != nil {
		e.logf("reconciling outcomes: reading the log: %v", err)
		return 0
	}
	// A short page means the end; the cursor wraps so the next pass starts
	// over and entries added or skipped earlier come round again.
	e.reconcile.setCursor(connID, next)

	// One bounded sweep per pass for rows stranded under a settled outcome,
	// rather than a query per settled transaction.
	//
	// Only on the UNSCOPED pass. The sweep is global by nature — it looks
	// for pending history rows, not for one connection's — so running it
	// from the checkout trigger would repeat the same global work once per
	// active connection while pretending to be scoped.
	if connID == 0 {
		e.repairPendingHistory(ctx)
	}

	now := e.now()
	resolved := 0
	for txID, group := range byTx {
		st := foldTxLog(group)
		if st.Terminal() {
			// Queued but already settled: a terminal written by a build that
			// predates the queue, or a dequeue that lost its process. Clear
			// the entry so the backlog shrinks rather than carrying a
			// permanent no-op.
			e.dequeueSettled(ctx, txID, st)
			continue
		}
		if !needsOracle(st.State) {
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

// maxReconcileBatch bounds one pass.
//
// Two different limits at once, and both matter. Memory: a pass materializes
// the groups it folds. And query PARAMETERS: the group fetch is an IN list,
// and an unbounded one eventually exceeds the driver's parameter limit — at
// which point reconciliation does not slow down, it STOPS, silently, exactly
// when the backlog is largest (PR #20 r1 MF1). Whatever a pass does not reach
// is picked up by the next one, so paging costs latency and never coverage.
const maxReconcileBatch = 200

// pendingGroups loads the unresolved transactions, from the QUEUE.
//
// The queue is the only thing that can answer this cheaply. Selecting by
// STATE cannot: every transaction keeps its `opened` row forever and every
// committed one keeps `commit_started`, so a state predicate matches the
// entire history — which is what the previous version did while looking
// selective, returning all 33 groups in lector's 32-settled-plus-one-pending
// probe.
//
// The queue holds only tx_ids with no terminal, so a normally-empty backlog
// costs one bounded indexed read and nothing else. The full group is then
// re-read for the ones it names, because the queue carries no state and the
// log stays the single source of truth.
func (e *Engine) pendingGroups(ctx context.Context, connID int64, limit int, after int64) (map[string][]*meta.TxOutcome, int64, error) {
	q := e.store.TxPending.OnCtx(ctx).OrderBy(dao.Asc(meta.TxPendByID))
	if connID != 0 {
		q = q.With(meta.TxPendConnID, connID)
	}
	if after > 0 {
		q = q.WithPredicate(dao.Gt(string(meta.TxPendID), after))
	}
	// The CALLER's bound, not this file's. Clamping to maxReconcileBatch
	// here would silently cap the read API at the reconciler's batch size,
	// making a larger backlog invisible past it with nothing saying so —
	// each caller already bounds itself for its own reason (the reconciler
	// to keep a pass bounded, the verb by MaxPendingLimit).
	if limit <= 0 {
		limit = maxReconcileBatch
	}
	queued, err := q.Limit(uint64(limit)).Select()
	if err != nil {
		return nil, 0, err
	}
	// The highest id READ, so the caller can continue past this page. Zero
	// when the page came back short, which means the cursor should wrap.
	var next int64
	if len(queued) == limit {
		for _, r := range queued {
			if r.ID > next {
				next = r.ID
			}
		}
	}
	byTx := map[string][]*meta.TxOutcome{}
	if len(queued) == 0 {
		return byTx, 0, nil
	}
	ids := make([]any, 0, len(queued))
	for _, r := range queued {
		ids = append(ids, r.TxID)
	}
	rows, err := e.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, ids...).Select()
	if err != nil {
		return nil, 0, err
	}
	for _, r := range rows {
		byTx[r.TxID] = append(byTx[r.TxID], r)
	}
	return byTx, next, nil
}

// needsOracle reports whether a nonterminal state can only be settled by
// asking the target.
//
// `opened` is excluded: a transaction that never reached commit_started
// cannot have committed, so there is nothing to ask. While its session is
// LIVE it belongs to the paths that own it — the boundary handler, the
// timeout sweep, the janitor — and terminating it from here would race a
// transaction that is about to commit perfectly normally.
//
// A prior-process `opened` row has no such owner, and is settled by
// RecoverStaleOpen at startup instead (PR #20 r0 MF1).
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
	switch {
	case errors.Is(err, dao.ErrNoRows):
		// The connection was DELETED, so no oracle can ever be consulted for
		// this entry again. Permanent, and named as such.
		return e.terminate(ctx, txID, st, meta.TxUnresolvable, meta.ReasonConnectionGone)
	case err != nil:
		// A meta-store failure proves nothing about the connection (PR #20
		// r0 SF3). Treating it as deletion would terminate a resolvable
		// transaction because a lookup blipped — the same fabrication the
		// unreachable-target branch exists to avoid.
		e.logf("reconciling %s: reading connection %d: %v", txID, st.ConnID, err)
		return false
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

// RecoverStaleOpen settles transactions this process inherited from a dead one.
//
// A crash after `opened` and before `commit_started` leaves a row no later
// owner exists for: the dead process's session, timeout reaper and boundary
// handler all went with it, so nothing would ever settle it and §7's "exactly
// one terminal" would be false for that transaction forever (PR #20 r0 MF1).
//
// The outcome is not a guess. A transaction that never reached commit_started
// cannot have committed — the target aborts it when the connection dies — so
// `rolled_back` is proven by the same reasoning Amendment 5 decision 2 uses
// to justify carrying no xid before that point.
//
// SYNCHRONOUS, and called before the daemon serves anything. That ordering is
// the whole safety argument: at this instant this process has no sessions, so
// every `opened` row in the store belongs to a process that is gone. There is
// no epoch to compare and no live row to misidentify, because a live row
// cannot exist yet. A periodic pass must never do this — by then the rows it
// would find are this process's own, live, and about to commit.
func (e *Engine) RecoverStaleOpen(ctx context.Context) int {
	if n := len(e.sessions.snapshot()); n != 0 {
		// Refuses rather than proceeds: the guarantee above is an ordering
		// one, and if it does not hold the sweep would roll back live work.
		e.logf("stale-transaction recovery skipped: %d session(s) already open, so an "+
			"`opened` row can no longer be assumed to belong to a dead process", n)
		return 0
	}
	// Startup recovery pages the WHOLE queue rather than one batch: it runs
	// once, before serving, and an inherited transaction it skipped would
	// have no other owner to settle it.
	byTx := map[string][]*meta.TxOutcome{}
	for cursor := int64(0); ; {
		page, next, perr := e.pendingGroups(ctx, 0, maxReconcileBatch, cursor)
		if perr != nil {
			e.logf("stale-transaction recovery: reading the log: %v", perr)
			return 0
		}
		for k, v := range page {
			byTx[k] = v
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	settled := 0
	for txID, group := range byTx {
		st := foldTxLog(group)
		if st.Terminal() || st.State != meta.TxOpened {
			continue
		}
		if e.terminate(ctx, txID, st, meta.TxRolledBack, meta.ReasonSessionClosed) {
			settled++
		}
	}
	if settled > 0 {
		e.logf("stale-transaction recovery settled %d transaction(s) left open by a previous process", settled)
	}
	return settled
}

// StartOutcomeReconciler runs the reconciler at startup and then on a ticker.
//
// Both are required (ADR-0074 §7): the startup pass is what recovers the
// crash window — it is the whole reason the log exists — and the periodic
// pass is what resolves entries whose target was down when the startup pass
// ran, and the live indeterminate commits that need no crash at all.
// The startup work is SYNCHRONOUS and the periodic pass is not.
//
// Startup recovery has to finish before the daemon accepts requests, because
// RecoverStaleOpen's correctness rests on this process having no sessions yet
// (see there). Running it in a goroutine would race the first client.
//
// A non-positive interval DISABLES the periodic pass and is a supported
// operator choice, not an error — ADR-0074 Amendment 4 A1 names the semantics:
// unset takes the default, zero or negative leaves startup and checkout
// reconciliation. Silently rewriting it to a minute, as this did, made the
// ratified configuration unreachable (PR #20 r0 MF5).
func (e *Engine) StartOutcomeReconciler(ctx context.Context, every time.Duration) {
	// Inherited transactions first: they are settled by proof, not by asking
	// anyone, and doing it before the oracle pass means that pass sees a
	// smaller backlog.
	e.RecoverStaleOpen(ctx)
	if n := e.ReconcileOutcomes(ctx); n > 0 {
		e.logf("startup reconciliation resolved %d transaction outcome(s)", n)
	}
	if every <= 0 {
		e.logf("periodic outcome reconciliation is disabled (interval %s); startup and "+
			"connection-checkout reconciliation remain active", every)
		return
	}
	go func() {
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
