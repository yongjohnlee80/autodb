package exec

import (
	"context"
	"fmt"
	"strings"

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
	case engine.MySQL:
		return mysqlDialect{}
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

// SessionGrammarVerifier is a target whose SESSION settings can change how SQL
// parses, and which can be asked whether the current session is safe to
// classify against.
//
// The classifier is a lexer: it decides where a statement ends and what its
// verb is by reading characters. A session setting that changes quoting or
// escaping changes what those characters MEAN, so a classification made under
// one setting and executed under another is not a stale answer — it is an
// answer to a different question.
//
// A target that cannot drift does not implement this. SQLite's grammar is
// fixed: there is no setting to check, and a verifier that returned nil would
// be claiming a check that did not happen.
type SessionGrammarVerifier interface {
	VerifySessionGrammar(ctx context.Context, q dao.Querier) error
}

// PerStatementGrammarVerifier is a target whose grammar must be re-checked on
// EVERY statement, inside the transaction that runs it.
//
// A DIFFERENT OBLIGATION FROM THE ONE ABOVE, though it asks the same question.
// PostgreSQL verifies each physical connection once, at establish time,
// through the pool's connect hook — so statements run in plain autocommit,
// which is what keeps transaction-prohibited DDL executable. MySQL has no
// per-connect seam in database/sql, so the only place it can hold the
// guarantee is inside the statement's own transaction, and the cost is that
// every statement runs in one.
//
// Two interfaces rather than one with a flag, because "can this session drift"
// and "where can I stand to check it" are separate facts, and a third engine
// could answer them independently — a driver with a connect hook and a stable
// grammar answers the first no and the second no; one with neither answers the
// first yes and the second yes.
type PerStatementGrammarVerifier interface {
	VerifyStatementGrammar(ctx context.Context, q dao.Querier) error
}

// lexerIncompatibleModes are the MySQL sql_mode flags that change how the
// classifier must read a statement.
//
// MOVED VERBATIM from dsn.go, not retyped. The first version of this line lost
// "ANSI" — a mode that implies ANSI_QUOTES — because it was written from
// memory of the switch it replaced rather than copied out of it. A refactor
// that narrows a security list while claiming to move it is the exact shape
// the sweep's behaviour-preservation rule exists to catch.
var lexerIncompatibleModes = []string{"NO_BACKSLASH_ESCAPES", "ANSI_QUOTES", "ANSI"}

// VerifySessionGrammar checks standard_conforming_strings.
//
// Off means a backslash escapes inside a single-quoted literal, which is the
// one setting that changes where a PostgreSQL statement ENDS — so the
// classifier's answer and the server's reading part company.
func (postgresDialect) VerifySessionGrammar(ctx context.Context, q dao.Querier) error {
	scs, err := scalarStringQ(ctx, q, "SHOW standard_conforming_strings")
	if err != nil {
		return fmt.Errorf("exec: reading standard_conforming_strings: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(scs), "on") {
		return fmt.Errorf("exec: session has standard_conforming_strings=off, which changes string parsing the classifier relies on")
	}
	return nil
}

// mysqlDialect carries what MySQL can do that the generic path cannot assume.
type mysqlDialect struct{}

// VerifySessionGrammar checks sql_mode for the flags that change parsing.
func (mysqlDialect) VerifySessionGrammar(ctx context.Context, q dao.Querier) error {
	mode, err := scalarStringQ(ctx, q, "SELECT @@SESSION.sql_mode")
	if err != nil {
		return fmt.Errorf("exec: reading sql_mode: %w", err)
	}
	up := strings.ToUpper(mode)
	for _, bad := range lexerIncompatibleModes {
		if strings.Contains(up, bad) {
			return fmt.Errorf("exec: session sql_mode contains %s, which changes SQL parsing the classifier relies on", bad)
		}
	}
	return nil
}

// VerifyStatementGrammar is the same check, at the only place MySQL can make
// it hold: inside the statement's own transaction.
func (d mysqlDialect) VerifyStatementGrammar(ctx context.Context, q dao.Querier) error {
	return d.VerifySessionGrammar(ctx, q)
}
