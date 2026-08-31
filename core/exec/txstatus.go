package exec

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The transaction outcome READ API — the R4/R5 seam (ADR-0074 Amendment 5).
//
// tx.status cannot be a projection over script_history: [history].enabled=false
// erases the rows, and a boundary-only BEGIN; COMMIT; never had one. Both are
// exactly the cases a user most needs answered, so the outcome log is the
// source and this is the only way to read it. No consumer queries the table
// directly — a consumer that folded the transition log itself would own a copy
// of the state machine, and copies drift.

// ErrNoSuchTx reports a transaction id with no progression.
//
// Distinct from pending ON PURPOSE. A typo'd or fabricated id answering
// "pending" would leave a caller waiting forever on a transaction that never
// existed, so absence is its own answer — and the write-ahead ordering in
// beginTx is what makes it truthful: the opened row is durable before the
// target is asked to begin anything, so zero rows PROVES nothing started.
//
// It is also what a caller gets for someone else's transaction. Answering
// "denied" there would confirm the id exists, turning the verb into a probe
// for which transactions are running; the same rule the connection surfaces
// already follow.
var ErrNoSuchTx = errors.New("exec: no such transaction")

// TxStatus is a transaction's progression, already folded.
type TxStatus struct {
	TxID   string
	State  meta.TxState
	Reason string
	// ConnID says WHICH target is involved. An operator reading a pending
	// list needs "what is stuck" and "on what" together; without it they
	// have to correlate against another surface to learn which database is
	// holding locks (ultron-prime, R4/R5 seam).
	ConnID int64
	UserID int64
	// Since is when the CURRENT state was recorded, Opened when the
	// transaction started. The gap between them is how long it has been
	// stuck, which is the number that decides whether to act.
	Since  time.Time
	Opened time.Time
}

// Terminal reports whether the outcome is settled.
//
// A METHOD rather than a field, deliberately: a precomputed bool can be
// constructed inconsistently — TxStatus{State: committed, Terminal: false}
// would compile and lie — and the point of folding the log in one place is
// that the derivation lives in one place too. Wire projections may precompute
// it, since a Lua or web client cannot call this.
func (s TxStatus) Terminal() bool { return s.State.IsTerminal() }

// DefaultPendingLimit bounds an unspecified request; MaxPendingLimit bounds
// any request, so a frontend cannot ask for the whole table.
const (
	DefaultPendingLimit = 100
	MaxPendingLimit     = 500
)

// TxOutcome returns one transaction's folded status.
func (e *Engine) TxOutcome(ctx context.Context, token, txID string) (TxStatus, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return TxStatus{}, err
	}
	rows, err := e.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, txID).Select()
	if err != nil {
		return TxStatus{}, fmt.Errorf("exec: reading the outcome log: %w", err)
	}
	if len(rows) == 0 {
		return TxStatus{}, fmt.Errorf("%w: %s", ErrNoSuchTx, txID)
	}
	st := foldTxLog(rows)
	if ident.Role() != "admin" && st.UserID != ident.UserID() {
		// Not a permission error: see ErrNoSuchTx.
		return TxStatus{}, fmt.Errorf("%w: %s", ErrNoSuchTx, txID)
	}
	return st, nil
}

// PendingOutcomes lists the transactions whose outcome is not yet settled,
// oldest first.
//
// Oldest first because the operator question is "what is stuck", and a limit
// that truncated the OLDEST would hide precisely the entries worth acting on.
func (e *Engine) PendingOutcomes(ctx context.Context, token string, limit int) ([]TxStatus, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultPendingLimit
	}
	limit = min(limit, MaxPendingLimit)

	// From the QUEUE, like the reconciler, and for the same reason: a
	// settled transaction keeps its `opened` and `commit_started` rows
	// forever, so reading the log and discarding the terminal groups in
	// memory is an O(all history) scan for an answer that is normally empty.
	// This one is worse than the reconciler's was, because a user can ask
	// for it (PR #20 r0 SF1 — I fixed the reconciler and left this behind).
	// The caller's limit is passed down, so a request for more than the
	// reconciler's batch size is not silently truncated to it — the two
	// paths bound the same query for different reasons and should not
	// borrow each other's number.
	byTx, err := e.pendingGroups(ctx, 0, limit)
	if err != nil {
		return nil, fmt.Errorf("exec: reading the outcome log: %w", err)
	}

	admin := ident.Role() == "admin"
	out := make([]TxStatus, 0, len(byTx))
	for _, group := range byTx {
		st := foldTxLog(group)
		if st.Terminal() {
			continue
		}
		if !admin && st.UserID != ident.UserID() {
			continue
		}
		out = append(out, st)
	}
	// Oldest by when the transaction OPENED, not by when its latest
	// transition was written: a stuck transaction that a sweep keeps
	// revisiting would otherwise look newer on every pass and sink down the
	// list exactly as it became more urgent.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Opened.Equal(out[j].Opened) {
			return out[i].Opened.Before(out[j].Opened)
		}
		return out[i].TxID < out[j].TxID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// foldTxLog collapses a transaction's transitions into its current status.
//
// The single place the progression is interpreted. Ordering is by seq rather
// than by created_at because seq is the progression's own order and is
// uniquely enforced by the store, whereas two transitions written in the same
// second share a timestamp and would order arbitrarily.
//
// A terminal wins over a later-seq nonterminal on the same transaction. That
// cannot arise from the writers as they stand — the store's terminal guard
// admits one terminal, and the reconciler stops at it — but a fold that
// reported "pending" for a transaction whose log contains "committed" would
// be the worst possible answer, so the invariant is enforced here rather than
// assumed.
func foldTxLog(rows []*meta.TxOutcome) TxStatus {
	if len(rows) == 0 {
		return TxStatus{}
	}
	sorted := make([]*meta.TxOutcome, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	first, last := sorted[0], sorted[len(sorted)-1]
	for _, r := range sorted {
		if meta.TxState(r.State).IsTerminal() {
			last = r
			break
		}
	}
	return TxStatus{
		TxID:   first.TxID,
		State:  meta.TxState(last.State),
		Reason: last.Reason,
		ConnID: first.ConnectionID,
		UserID: first.UserID,
		Since:  time.Unix(last.CreatedAt, 0),
		Opened: time.Unix(first.CreatedAt, 0),
	}
}
