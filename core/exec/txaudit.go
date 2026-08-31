package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The transaction outcome log — the writer half of ADR-0074 §7 rev 2.
//
// Every transaction's life is recorded as an APPEND-ONLY progression of
// transitions rather than as a mutable status column, because a status column
// can only ever hold the last thing a writer believed, and the whole problem
// this solves is that the last thing a writer believed may have been written
// by a process that then died, or by a resolver that was racing another one.
//
// Two rules make the log trustworthy, and both are enforced by the STORE
// (core/meta v5), not here:
//
//   - append-only: UNIQUE(tx_id, seq), so a transition cannot be rewritten;
//   - exactly-one-terminal: UNIQUE(tx_id) WHERE terminal, so two resolvers
//     that both learn an outcome cannot both append one.
//
// This file's job is to append correctly and to interpret a collision
// correctly. Those are different things and the second is the subtle one: a
// terminal collision is NOT an error. It means another resolver — the
// boundary handler, the timeout reaper, the reconciler — got there first, and
// since the store admits exactly one terminal, whichever one landed IS the
// outcome. Retrying would either duplicate it or, worse, contradict it.

// txTransition is one appendable step in a transaction's progression.
type txTransition struct {
	txID   string
	state  meta.TxState
	reason string // only meaningful for the terminal states

	userID       int64
	connectionID int64
	historyID    int64  // 0 when history is off or there is no statement
	targetXID    string // the target's txid_current(), captured at BEGIN
}

// maxSeqAttempts bounds the seq-allocation retry.
//
// A collision here means a genuinely concurrent appender, and the next read
// sees its row, so the loop converges in one extra pass under any realistic
// contention. The bound exists so that a pathological case fails loudly
// instead of spinning: the outcome log is on the teardown path, and a writer
// that will not terminate there holds up a rollback.
const maxSeqAttempts = 8

// appendTxOutcome appends one transition to the log.
//
// It is deliberately NOT gated on e.history. History is an optional
// convenience that an operator may switch off; the outcome log is the record
// of whether a transaction's effects survived, and switching it off would
// mean the engine could not answer that question at all — which is the case
// lector's Amendment-4 MF1 pinned as unacceptable. A boundary-only
// `BEGIN; COMMIT;` writes no history row and still gets a full progression
// here.
func (e *Engine) appendTxOutcome(ctx context.Context, t txTransition) error {
	if t.txID == "" {
		// A transition with no correlation id is unresolvable by anything
		// downstream; recording it would grow the log without growing what
		// the log can answer.
		return fmt.Errorf("exec: appending a %s transition without a tx id", t.state)
	}

	terminal := t.state.IsTerminal()
	for attempt := 0; attempt < maxSeqAttempts; attempt++ {
		head, err := e.txLogHead(ctx, t.txID)
		if err != nil {
			return err
		}
		// A terminal ENDS the trail, for every appender and not only for
		// another terminal (PR #20 r0 MF2).
		//
		// The store's partial index refuses a second terminal but permits a
		// nonterminal after one, and that gap is reachable in production:
		// the reconciler can resolve a durable commit_started while
		// finishTx is between the target's answer and its own append, and
		// finishTx's unknown_pending would then land AFTER the committed row.
		// The result is an append-only trail that does not end in its
		// terminal — foldTxLog reads it correctly, but a forgiving reader
		// does not restore the invariant, it just hides that it was broken.
		//
		// Re-read on every pass rather than once before the loop: another
		// resolver may have finished while this one was retrying a seq
		// collision. Together with the seq retry that closes the race in
		// both directions — if the nonterminal wins the seq it is simply
		// followed by the terminal; if the terminal wins, the retrying
		// nonterminal reads it here and stops.
		if head.terminal {
			return nil
		}
		// A progression does not repeat itself. The retrying paths — the
		// timeout sweep, the janitor, the reconciler — revisit the same
		// undetermined transaction on every pass, and without this each pass
		// would append another identical unknown_pending. The log would grow
		// without learning anything, which is the opposite of what an
		// append-only log is for.
		if !terminal && head.state == t.state && head.reason == t.reason {
			return nil
		}

		// The transition and its projection go in ONE meta transaction
		// (PR #20 r0 MF3).
		//
		// They were two operations, and a crash between them left the source
		// of truth terminal while history still said ok_pending_commit —
		// permanently, because reconciliation folds the terminal and skips
		// the group before any repair could run. Both writes are in the same
		// store, so atomicity is available and there is no reason to accept
		// a window that nothing heals.
		//
		// A settled transaction settles its statements HERE rather than at
		// each call site, so that every resolver projects: the boundary
		// handler, the timeout sweep, the janitor and the reconciler all
		// reach this code, and one that forgot would leave rows stuck
		// pending forever.
		err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
			if _, ierr := e.store.TxOutcomes.On(tx).
				Set(meta.TxOutTxID, t.txID).
				Set(meta.TxOutSeq, head.nextSeq).
				Set(meta.TxOutState, string(t.state)).
				Set(meta.TxOutReason, t.reason).
				Set(meta.TxOutUserID, t.userID).
				Set(meta.TxOutConnID, t.connectionID).
				Set(meta.TxOutHistoryID, t.historyID).
				Set(meta.TxOutTargetXID, t.targetXID).
				Set(meta.TxOutCreatedAt, e.now().Unix()).
				Insert(); ierr != nil {
				return ierr
			}
			if terminal {
				return e.projectHistoryTx(tx, t.txID, t.state)
			}
			return nil
		})
		switch {
		case err == nil:
			// Fired from the primitive, the instant this transition is
			// durable — see txBoundaryHook for why it does not live at the
			// call site.
			boundaryReached(txBoundaryPoint("appended:" + string(t.state)))
			return nil
		case !errors.Is(err, dao.ErrDuplicate):
			return fmt.Errorf("exec: appending %s for %s: %w", t.state, t.txID, err)
		case terminal:
			// Either index could have refused this. Both readings mean the
			// same thing for a terminal — someone else is writing this
			// transaction's log right now — so re-read and let the loop's
			// own terminal check decide, rather than guessing which
			// constraint fired from an error string.
			continue
		default:
			// A nonterminal lost a seq race. Recompute and append after the
			// winner; the progression stays ordered.
			continue
		}
	}
	return fmt.Errorf("exec: could not append %s for %s after %d attempts: the log is under sustained concurrent write",
		t.state, t.txID, maxSeqAttempts)
}

// txLogHead summarizes a transaction's progression: where the next row goes,
// what the latest transition says, and whether a terminal has landed.
//
// It reads the whole progression rather than asking the store for a maximum
// because the progression is bounded by the state machine — opened,
// commit_started, at most one unknown_pending, one terminal — so this is a
// handful of rows, and folding them in Go keeps the terminal test on the same
// TxState.IsTerminal() the rest of the engine uses instead of duplicating the
// terminal vocabulary into a SQL predicate that could drift from it.
func (e *Engine) txLogHead(ctx context.Context, txID string) (txLogSummary, error) {
	rows, err := e.store.TxOutcomes.OnCtx(ctx).With(meta.TxOutTxID, txID).Select()
	if err != nil {
		return txLogSummary{}, fmt.Errorf("exec: reading the outcome log for %s: %w", txID, err)
	}
	head := txLogSummary{nextSeq: 1}
	var latest int64
	for _, r := range rows {
		if r.Seq >= head.nextSeq {
			head.nextSeq = r.Seq + 1
		}
		if meta.TxState(r.State).IsTerminal() {
			head.terminal = true
		}
		// Latest by seq, not by created_at: seq is the progression's own
		// order and is uniquely enforced, whereas two transitions written in
		// the same second share a timestamp and would order arbitrarily.
		if r.Seq >= latest {
			latest, head.state, head.reason = r.Seq, meta.TxState(r.State), r.Reason
		}
	}
	head.rows = len(rows)
	return head, nil
}

// txLogSummary is one transaction's progression folded to what a writer needs.
type txLogSummary struct {
	nextSeq  int64
	state    meta.TxState // the latest transition, "" when the log is empty
	reason   string
	terminal bool
	rows     int
}

// noteTxOutcome appends a transition and reports failure to the log rather
// than to the caller.
//
// Used on teardown paths, where the alternative is worse in both directions:
// returning the error aborts a rollback that must finish, and the transition
// is evidence whose loss should be visible. Callers that CAN handle the error
// — the boundary handler, which is still on the user's call — use
// appendTxOutcome directly.
func (e *Engine) noteTxOutcome(ctx context.Context, t txTransition) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := e.appendTxOutcome(actx, t); err != nil {
		e.logf("recording the %s transition for %s failed: %v", t.state, t.txID, err)
	}
}

// txStateFor and txOutcomeReason turn finalize()'s outcome word plus the raw
// error into the logged state and its reason.
//
// The classification lives HERE, not in finalize, because the state vocabulary
// is the outcome log's and a control path should not have to know it
// (ultron-prime, R4/R5 seam, A5 plumbing). finalize returns the word and the
// error; the error crosses the seam as data and is classified once, in one
// place, by code that can be tested against error values without driving a
// real commit.
func txStateFor(outcome string, err error) meta.TxState {
	switch outcome {
	case "committed":
		return meta.TxCommitted
	case "rolled_back", "rollback_failed":
		return meta.TxRolledBack
	case "unknown_pending":
		return meta.TxUnknownPending
	case "commit_failed":
		// The split is by whether the SERVER ANSWERED, not by whether golib
		// recognised the error.
		//
		// The tempting shortcut is to call every commit_failed definite, on
		// the reasoning that golib maps each unprovable case to
		// ErrTxOutcomeUnknown, so anything reaching here must be provable.
		// That leans on a DEPENDENCY's classification being exhaustive in
		// order to assert a terminal — and if it is ever incomplete, the
		// engine fabricates a rolled_back for a commit that actually landed.
		// Fabricating a terminal is the one thing §7's invariant forbids
		// outright, so the doubt resolves toward the nonterminal.
		//
		// A *pgconn.PgError means the server received the COMMIT, evaluated
		// it and refused it — a deferred constraint violation being the
		// ordinary case. Nothing was applied, and that is the server's own
		// word rather than an inference. Anything else is a transport or
		// context failure with no answer from the server at all, which is
		// precisely the shape of an unknown.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return meta.TxRolledBack
		}
		return meta.TxUnknownPending
	}
	// An unrecognised classification is itself an unknown outcome.
	// Terminating it honestly beats guessing at one of the definite states.
	return meta.TxUnresolvable
}

func txOutcomeReason(outcome string, err error) string {
	switch outcome {
	case "rollback_failed":
		return meta.ReasonSessionClosed
	case "commit_failed":
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// A definite refusal; the state says rolled_back and the reason
			// would add nothing the error text does not already carry.
			return ""
		}
		return meta.ReasonUnanswered
	}
	return ""
}
