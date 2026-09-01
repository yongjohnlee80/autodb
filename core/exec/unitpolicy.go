package exec

import (
	"context"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
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
	return UnitPolicy{Role: v.Role, ReadOnly: !v.MayWrite}, nil
}

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
