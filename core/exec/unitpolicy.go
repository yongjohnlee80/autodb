package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE SHARED EXECUTION-UNIT POLICY (ADR-0075 Amendment 4's F3a seam
// condition, lector's ruling).
//
// One engine-owned decision, resolved FRESH for every execution unit, that
// every caller reuses — the simple path today, the wire's segments when F1
// lands, every Execute when F2 does. It is deliberately not a wrapper on any
// one of them: a policy that lives in a caller has to be rebuilt by the next
// caller, and the two then disagree about who may write.
//
// WHY FRESH EVERY TIME. A session outlives the call that opened it, so the
// role it was opened under is a historical fact rather than a current one. A
// user demoted from editor to reader mid-session keeps whatever authority was
// cached until something re-reads it, and "something" must not mean "the next
// background sweep" — that is a window measured in the janitor's interval,
// during which the demoted user writes.
//
// WHY THE SERVER ENFORCES IT. autodb's classifier decides what a statement
// IS, and it is right about `SELECT audit_and_return(...)`: that statement is
// a read. If the function writes, the write happens anyway. No amount of
// classification catches it, because there is nothing in the text to catch —
// so the boundary has to be one PostgreSQL itself holds, which is a read-only
// transaction and a raw 25006 coming back.

// UnitPolicy is the decision for one execution unit.
type UnitPolicy struct {
	// Role is the effective account role at this instant, for the audit.
	Role string
	// Ident is the caller, from the same read that decided the policy. A
	// second read would be a second answer.
	Ident auth.Identity
	// MayWrite reports whether the effective role clears the write floor.
	// The inverse of ReadOnly, kept as its own field because a caller
	// authorizing a statement asks the positive question and one that is
	// wrapping asks the negative — and `!ReadOnly` at an authorization site
	// reads like a double negative nobody checks.
	MayWrite bool
	// ReadOnly requires the unit to run inside a server-enforced read-only
	// transaction. It is derived from the SAME verdict the janitor's
	// authority re-check uses, so the foreground and the background cannot
	// disagree about who may write.
	ReadOnly bool
}

// resolveUnitPolicy reads the caller's standing authority and derives the
// unit's policy from it.
//
// It shares auth.ResolveStanding with the janitor deliberately. Two
// implementations of "may this caller write" drift in the direction nobody
// notices: a sweep stricter than the foreground merely kills sessions, while a
// foreground laxer than the sweep serves statements the sweep would have
// refused — and the second is the one that loses data.
//
// A standing failure is returned as ErrDenied rather than as a policy: a
// caller whose authority has ended does not get a read-only unit, they get
// nothing. A STORE failure is returned as itself, because "the database is
// unreachable" must not be reported as "you are not allowed".
func (e *Engine) resolveUnitPolicy(ctx context.Context, ref auth.AuthorityRef, userID, connID int64) (UnitPolicy, error) {
	v, err := e.auth.ResolveStanding(ctx, ref, userID, connID)
	if err != nil {
		return UnitPolicy{}, err
	}
	if !v.Standing {
		return UnitPolicy{}, auth.ErrDenied
	}
	return UnitPolicy{Role: v.Role, MayWrite: v.MayWrite, ReadOnly: !v.MayWrite, Ident: v.Identity}, nil
}

// tokenUnitPolicy resolves the policy for a caller holding a session token.
//
// The token path for now. When F1's wire execution lands, its caller will
// resolve from the session's own PAT-backed AuthorityRef and hand the policy
// in — same resolver, one more kind of credential, no second decision. It is
// not parameterised yet because the wire path's shape is not designed, and a
// signature built for a caller that does not exist is a guess.
func (e *Engine) tokenUnitPolicy(ctx context.Context, token string, connID int64) (UnitPolicy, error) {
	ident, sessID, err := e.auth.SessionRef(ctx, token)
	if err != nil {
		return UnitPolicy{}, err
	}
	return e.resolveUnitPolicy(ctx, auth.SessionAuthority(sessID), ident.UserID(), connID)
}

// ErrReadOnlyUnenforceable refuses a read-only unit on a target that cannot
// host a transaction.
//
// FAIL CLOSED. The alternative is to run the statement unwrapped and hope the
// classifier caught everything — which is the whole thing this policy exists
// because it cannot. A reader whose target has no transactions gets an error
// they can act on; a reader whose write silently reaches the server gets
// nothing, and neither does the operator.
var ErrReadOnlyUnenforceable = errors.New("exec: this connection cannot enforce read-only execution")

// applyTo forces the transaction's access mode when the policy says read-only.
//
// FORCED, over both the client's default and an explicit request. A per-
// session read-only DEFAULT is not enough and this is the reason: the
// transaction-control parser accepts `BEGIN READ WRITE` and hands the access
// mode straight back, so a reader who merely asks gets their write
// transaction. The gate names that path explicitly, and it is the one an
// implementation is most likely to leave open, because a default LOOKS like
// enforcement until somebody asks for the other thing.
//
// The returned bool reports whether the mode was overridden, so the caller can
// audit an attempted upgrade rather than silently downgrading it. A refusal
// that says nothing teaches a reader their write transaction succeeded.
func (p UnitPolicy) applyTo(opts *dao.TxOptions) (overridden bool) {
	if !p.ReadOnly {
		return false
	}
	if opts.Access == dao.TxReadOnly {
		return false
	}
	opts.Access = dao.TxReadOnly
	return true
}

// wrapReadOnly runs one autocommit unit inside a server-enforced read-only
// transaction, or explains why it cannot.
//
// A unit with no pinned transaction runs on a pooled connection, where nothing
// constrains it but the classifier — and the classifier is RIGHT that
// `SELECT writes_a_row()` is a read. The write is in the function body, so the
// boundary has to be one the server holds.
//
// Called AFTER the attempt record, because opening a transaction is a
// target-visible effect and attempt-before-effect has no exception for effects
// that write nothing.
//
// Returns (tx, release, nil) when wrapped, (nil, nil, nil) when the guarantee
// is unavailable but not promised, and an error when it is unavailable and
// was.
func (e *Engine) wrapReadOnly(
	ctx context.Context, target dao.DataConn, connRow *meta.Connection,
	ident auth.Identity, connID int64, ip, sqlText string, pol UnitPolicy,
) (dao.TxConn, func(), error) {

	beginner, ok := target.(dao.TxBeginner)
	if !ok {
		// THE TARGET CANNOT HOST A READ-ONLY TRANSACTION, and what to do
		// about that depends on whether the guarantee was PROMISED.
		//
		// On the session profile it was: that is the front door's surface,
		// where a reader is told the database itself enforces the boundary.
		// Running unwrapped there would make the promise false while looking
		// identical, so it fails closed.
		//
		// On v1compat it was not. SQLite has no per-transaction read-only
		// mode, so refusing would take the reader role away from every such
		// target for a guarantee never offered on it — and the classifier
		// remains exactly the boundary it has always been. The unit is
		// AUDITED so the gap is visible rather than inferred from a driver's
		// capabilities.
		if e.profileFor(connRow) == ProfileSession {
			return nil, nil, e.reject(ctx, ident, connID, ip, sqlText, ErrReadOnlyUnenforceable)
		}
		e.auditBounded(ctx, ident.UserID(), ip, "readonly_unenforced",
			fmt.Sprintf("conn %d: role %s: this target cannot host a read-only transaction; "+
				"the statement ran under classifier enforcement only", connID, pol.Role))
		return nil, nil, nil
	}

	rotx, err := beginner.BeginTx(ctx, dao.TxOptions{Access: dao.TxReadOnly})
	if err != nil {
		return nil, nil, e.reject(ctx, ident, connID, ip, sqlText, err)
	}
	// ROLLED BACK, never committed: a read-only transaction has nothing to
	// commit, and a rollback cannot return an ambiguous outcome — so the
	// cleanup path has no case where the engine has to wonder what happened.
	release := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
		defer cancel()
		if ctxTx, okc := rotx.(dao.ContextTxConn); okc {
			_ = ctxTx.RollbackContext(cctx)
			return
		}
		_ = rotx.Rollback()
	}
	return rotx, release, nil
}
