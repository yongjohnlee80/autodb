package exec

import (
	"context"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// Target capabilities, asked for rather than assumed.
//
// WHY AN INTERFACE AND NOT THE PREDICATE IT REPLACES. core/engine's
// predicates answer "can this engine do X"; that was the right first move and
// it removed the identity tests. It does not remove the second half of the
// problem: the code that KNOWS HOW to do X is still sitting in the generic
// path, guarded by the answer. armServerBelt asked HasServerStatementTimeout
// and then wrote `SET LOCAL idle_in_transaction_session_timeout` — PostgreSQL
// statement text, in a function that takes any engine.
//
// A capability interface moves the how to the side that has it. The generic
// path asks whether the target has a belt and hands it the deadline; what the
// belt is made of stops being core/exec's business, which is what makes a
// third engine a new implementation rather than a new branch.
//
// OPTIONAL, NEVER PART OF A MANDATORY CORE, per golib's own convention for
// this pattern: a capability is a separate interface a dialect either
// implements or does not, so adding one cannot break an existing implementor
// and a dialect that lacks it says so by not implementing it — not by
// implementing it as a no-op, which is the shape that hides an absence.

// StatementTimeoutBelt is a target that can bound an idle transaction on its
// own side.
//
// THE BELT IS A SECOND LAYER, not the primary bound. The engine's reaper is
// the primary one; this exists because a client that stops reading leaves the
// engine's deadline running in a process the target cannot see, and a
// server-side bound survives that. A target without one is not broken — it has
// one layer instead of two, and the caller carries on.
type StatementTimeoutBelt interface {
	// ArmIdleTransactionBelt sets the target's own idle-in-transaction bound
	// on tx, for seconds. It is called INSIDE the transaction it bounds, so
	// the setting is scoped to it and needs no unwind.
	ArmIdleTransactionBelt(ctx context.Context, tx dao.TxConn, seconds int) error
}

// dialectFor resolves the capability set for an engine.
//
// Returns nil for an engine with no dialect of its own, and every capability
// probe treats nil as "has none" — which is the same answer the type assertion
// gives for a dialect that implements nothing, so the absent case has one
// shape rather than two.
func dialectFor(n engine.Name) any {
	switch n {
	case engine.Postgres:
		return postgresDialect{}
	}
	return nil
}

// postgresDialect carries what PostgreSQL can do that the generic path cannot
// assume.
type postgresDialect struct{}

// ArmIdleTransactionBelt sets idle_in_transaction_session_timeout.
//
// SET LOCAL, so it dies with the transaction: the connection returns to the
// pool carrying nothing, which is why this needs no unwind and why arming it
// on a POOLED connection would be a different and worse thing.
func (postgresDialect) ArmIdleTransactionBelt(ctx context.Context, tx dao.TxConn, seconds int) error {
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = '%ds'", seconds))
	return err
}
