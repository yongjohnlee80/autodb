package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Multi-statement scripts (Objective 5): a buffer holding several
// statements runs them IN ORDER, one at a time, through the identical
// single-statement path — so each one is classified, authorized,
// WHERE-guarded, audited and recorded in history on its own. Splitting
// changes how much you may submit in one call; it never changes what the
// engine agrees to run.
//
// The result of the LAST statement is what comes back: that is the one
// the user is looking at. A failure stops the script there — the
// statements before it already ran and are already recorded, which the
// error says explicitly so nobody assumes a rollback happened.

// ScriptResult is the outcome of running a multi-statement script.
type ScriptResult struct {
	// Last is the final statement's result (nil when none produced one).
	Last *Result
	// Statements is how many ran successfully.
	Statements int
	// FailedAt is the 1-based index of the statement that failed, 0 if
	// none did.
	FailedAt int
}

// ExecuteScript runs every statement in sqlText sequentially and returns
// the last result. A single statement behaves exactly like Execute.
func (e *Engine) ExecuteScript(ctx context.Context, token string, connID int64, sqlText, ip string) (*ScriptResult, error) {
	// Authorization and existence checks happen per statement inside run,
	// but the SPLIT needs the connection's dialect, and learning that must
	// not leak a connection's existence to an ungranted caller (R13).
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return nil, err
	}
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		return nil, auth.ErrDenied // never disclose which connections exist
	}

	parts, err := SplitStatements(sqlText, connRow.Engine == engine.MySQL)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, ErrEmptyStatement
	}

	out := &ScriptResult{}
	for i, stmt := range parts {
		res, rerr := e.Execute(ctx, token, connID, stmt, ip)
		if rerr != nil {
			out.FailedAt = i + 1
			if i == 0 {
				return out, rerr
			}
			// Statements before this one RAN. Say so — a script is not a
			// transaction, and a silent partial application is the kind of
			// thing an operator finds out about later.
			return out, fmt.Errorf("statement %d of %d failed (the %d before it already ran): %w",
				i+1, len(parts), i, rerr)
		}
		out.Statements++
		out.Last = res
	}
	return out, nil
}

// compile-time proof the errors above are the core's own.
var _ = errors.Is

// ExecuteScriptAtomic runs a script, and runs it INSIDE ONE TRANSACTION when
// the script asks for one (ADR-0074, R5 gate).
//
// This is the sugar that makes `BEGIN; …; COMMIT;` mean what a person typing
// it into a query editor believes it means. Without it the statements are
// independent: ExecuteScript sends each through the stateless path, so on a
// v1compat connection the BEGIN is refused outright, and on any pooled
// connection the boundary would not be honoured even if it were admitted —
// each statement can land on a different physical connection, so the COMMIT
// would commit nothing the BEGIN opened. "A script is not a transaction" is
// the correct default and stays the default; it stops being correct the
// moment the script itself says otherwise.
//
// A script with no transaction control behaves EXACTLY as before, statement
// by statement, unchanged. Only a script that contains a boundary takes the
// session path.
//
// Atomicity comes from the session's own close: the ephemeral session is
// closed on every exit path, and closing a session with an open transaction
// rolls it back (R3). So a script that opens a transaction and then fails —
// or simply never commits — leaves nothing applied, without this function
// needing to reason about which statement failed or issue a rollback itself.
func (e *Engine) ExecuteScriptAtomic(ctx context.Context, token string, connID int64, sqlText, ip string) (*ScriptResult, error) {
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return nil, err
	}
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		return nil, auth.ErrDenied // never disclose which connections exist
	}
	mysql := connRow.Engine == engine.MySQL
	parts, err := SplitStatements(sqlText, mysql)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, ErrEmptyStatement
	}
	if !scriptOpensATransaction(parts, mysql) {
		return e.ExecuteScript(ctx, token, connID, sqlText, ip)
	}

	sess, err := e.openSession(ctx, token, connID, ip)
	if err != nil {
		return nil, err
	}
	// The close is the atomicity, and it is the ENGINE's own close: the
	// session belongs to this call, so cleaning it up must not depend on the
	// caller's token still authenticating. Going back through the public
	// CloseSession meant a logout or a revocation during a long statement
	// made cleanup fail authentication — leaving the ephemeral session, and
	// the transaction pinned to it, registered and holding a connection.
	// An engine does not ask permission to clean up after itself.
	//
	// It runs on every path: success, failure, and a script that opened a
	// transaction and simply stopped. A close with an open transaction rolls
	// it back, and closeSession is synchronous, so cleanup is complete before
	// this function returns.
	defer e.closeSession(context.WithoutCancel(ctx), sess, ip, "atomic-script")

	// appliedOutside counts statements that completed while NO transaction
	// was open. They are applied, and a rollback will not remove them.
	var appliedOutside int
	out := &ScriptResult{}
	for i, stmt := range parts {
		inTxBefore := sess.inTransaction()
		res, rerr := e.SessionExecute(ctx, token, sess.id, stmt, ip)
		if rerr != nil {
			out.FailedAt = i + 1
			return out, e.atomicFailure(sess, i, len(parts), appliedOutside, rerr)
		}
		// A statement is "outside" when no transaction was open before it
		// AND it did not open one. BEGIN itself is neither applied nor lost.
		if !inTxBefore && !sess.inTransaction() {
			appliedOutside++
		}
		out.Statements++
		out.Last = res
	}
	return out, nil
}

// atomicFailure words the failure by what is ACTUALLY still applied.
//
// The first version of this promised "nothing in this script was applied" for
// every failure on the session path, which was false whenever the script had
// statements outside its transaction: the session path is taken when a
// boundary appears ANYWHERE, so `INSERT …; BEGIN; …; COMMIT;` runs that first
// INSERT with no transaction around it, and a rollback later cannot remove
// it. A promise of atomicity that is sometimes untrue is worse than none —
// it is exactly the assurance someone would act on.
//
// Whether a transaction is open is asked of the SESSION rather than tracked
// here. A second model of the transaction state is a second thing that can be
// wrong, and this one would be wrong precisely when it mattered.
func (e *Engine) atomicFailure(sess *session, idx, total, appliedOutside int, cause error) error {
	stmt := idx + 1
	if !sess.inTransaction() {
		// No transaction was open when this failed — either the script had
		// not opened one yet, or it had already committed. Nothing is being
		// rolled back, so nothing may claim to be.
		return fmt.Errorf("statement %d of %d failed with no transaction open; the %d statement(s) "+
			"before it already ran and are applied: %w", stmt, total, idx, cause)
	}
	if appliedOutside == 0 {
		// The whole script is one transaction. Here the strong promise is
		// true, and it is worth making plainly.
		return fmt.Errorf("statement %d of %d failed; the transaction was rolled back "+
			"and nothing in this script was applied: %w", stmt, total, cause)
	}
	return fmt.Errorf("statement %d of %d failed; the transaction was rolled back, but %d "+
		"statement(s) ran OUTSIDE it and are still applied: %w", stmt, total, appliedOutside, cause)
}

// scriptOpensATransaction reports whether any statement in the script is a
// transaction boundary.
//
// It asks the CLASSIFIER rather than looking for the word BEGIN, because the
// classifier is what the engine will use when it runs these statements, and
// two different readings of the same text is how a script gets admitted down
// one path and executed down another. A statement that will not classify is
// treated as not-a-boundary and left to fail on its own merits further down,
// where the error can name the statement.
func scriptOpensATransaction(parts []string, mysql bool) bool {
	for _, stmt := range parts {
		st, err := Classify(stmt, mysql)
		if err != nil {
			continue
		}
		if st.Class == ClassControl && txControlVerbs[st.Verb] {
			return true
		}
	}
	return false
}
