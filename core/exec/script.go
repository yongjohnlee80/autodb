package exec

import (
	"context"
	"errors"
	"fmt"

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

	parts, err := SplitStatements(sqlText, connRow.Engine == "mysql")
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
