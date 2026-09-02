package exec

import (
	"context"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// EXECUTION ON A WIRE SESSION (F1's engine side).
//
// A front-door session has no token. Its authority is a PAT, established once
// at row 2.7 and re-checked from the token's own row ever after — so every
// entry point here starts from the session's AuthorityRef rather than from
// something the caller is presenting.
//
// WHAT IS SHARED AND WHAT IS NOT, deliberately. The RESOLUTION differs, and
// legitimately: a session token resolves through a session row, a PAT through
// the token record, and neither can express the other's checks. Everything
// after is the same code — the same UnitPolicy, the same admission, the same
// executors, the same read-only wrap. That is the seam ADR-0075 Amendment 4
// requires and the reason this file is thin: a wire-shaped copy of the
// execution pipeline is exactly what it forbids.

// WireExecute runs one statement on a front-door session.
//
// The listener will call this once F0e lands; the cells drive it directly,
// which is the same thing minus a socket. Deliberately not waiting for the
// listener: what this proves is the CREDENTIAL-KIND seam — that a PAT-backed
// session reaches the same policy — and a socket adds nothing to that claim.
func (e *Engine) WireExecute(ctx context.Context, id SessionID, userID int64, sqlText, ip string) (*Result, error) {
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return nil, err
	}
	// One in-flight statement per session, claimed before any work, exactly
	// as the token path claims it.
	if err := s.begin(); err != nil {
		return nil, err
	}
	closeAfterRelease := false
	defer func() {
		s.finish()
		if closeAfterRelease {
			e.finishClosing(context.WithoutCancel(ctx), s)
		}
	}()
	return e.wireExecuteClaimed(ctx, s, sqlText, ip, &closeAfterRelease)
}

// wireExecuteClaimed is WireExecute AFTER the session claim: the caller holds
// s.begin() and owns s.finish(). It exists so WireQuery can keep ONE claim
// across gate, dispatch, every emit, and the status read (lector PR #48 r0
// MF1) — a WireQuery built on WireExecute released the claim in
// WireExecute's own defer, before the first emit, and a callback that
// re-entered the engine ran a second statement where ErrSessionBusy was
// owed. closeAfterRelease is the caller's flag because the caller's defer is
// the one that runs after release.
func (e *Engine) wireExecuteClaimed(ctx context.Context, s *session, sqlText, ip string, closeAfterRelease *bool) (*Result, error) {
	pol, err := e.wireAdmit(ctx, s, sqlText, ip, closeAfterRelease)
	if err != nil {
		return nil, err
	}
	// A postgres WIRE session pins its backend before ANY statement can open a
	// transaction, on this seam as on WireQuery: a BEGIN that opened on the pool
	// while later raw statements ran on the pinned connection would put the
	// client's statements outside the transaction it believes it is in.
	if connRow, cerr := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get(); cerr == nil && connRow.Engine == "postgres" {
		if _, perr := e.pinWireSession(ctx, s, connRow); perr != nil {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
		}
	}
	return e.executeSessionUnit(ctx, s, pol, sqlText, ip, true)
}

// wireAdmit is the wire path's preamble, shared by the decoded and the raw
// producers: the session must be open, and THE POLICY is resolved fresh from
// the session's own authority — the credential is a PAT and it is re-read from
// the PATs table, not the sessions table. A transaction whose authority has
// changed underneath it is rolled back and the statement refused.
func (e *Engine) wireAdmit(ctx context.Context, s *session, sqlText, ip string, closeAfterRelease *bool) (UnitPolicy, error) {
	if s.get() != sessOpen {
		return UnitPolicy{}, ErrSessionNotFound
	}
	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return UnitPolicy{}, perr
	}
	demoted, derr := e.enforceTransactionAuthority(ctx, s, pol, ip)
	if derr != nil {
		*closeAfterRelease = e.transferDemotionClose(s, ip)
		return UnitPolicy{}, e.rejectSession(ctx, s, pol.Ident, ip, sqlText,
			fmt.Errorf("%w: rollback cleanup failed: %v", ErrTxAuthorityChanged, derr))
	}
	if demoted {
		return UnitPolicy{}, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAuthorityChanged)
	}
	return pol, nil
}

// executeSessionUnit is the shared token/PAT session pipeline after one fresh
// policy has been resolved and transaction authority has been preflighted.
// Control authorization remains surface-owned because its two independent
// gates are load-bearing P2 evidence; ordinary execution shares everything.
func (e *Engine) executeSessionUnit(
	ctx context.Context, s *session, pol UnitPolicy, sqlText, ip string, wire bool,
) (*Result, error) {

	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if err != nil {
		return nil, auth.ErrDenied // never disclose which connections exist
	}

	// Reject oversized input BEFORE classification or control routing, for the
	// same reason the token path does (engine.go, lector M4 r2 must-fix #2):
	// the audit record must equal exactly what ran, and an unaudited tail must
	// never execute. The wire path omitted this, so a statement of any size
	// reached the classifier here while the identical statement was refused on
	// the token path.
	//
	// Placed above Classify so it also covers CONTROL statements: wireControl
	// is reached only through the routing below, so a gate here is the single
	// point that governs both.
	if len(sqlText) > e.maxStatementBytes {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrScriptTooLarge)
	}

	stmt, cerr := Classify(sqlText, connRow.Engine == "mysql")
	if cerr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, cerr)
	}

	// Transaction control is a state transition, routed before the execution
	// pipeline is entered at all — the same routing the token path does, for
	// the same reason (ADR-0074 §3).
	if stmt.Class == ClassControl {
		if wire {
			return e.wireControl(ctx, s, connRow, stmt, pol, sqlText, ip)
		}
		return e.tokenControl(ctx, s, connRow, stmt, pol, sqlText, ip)
	}

	// The statement's own class, authorized against the SAME verdict the
	// policy came from rather than by a second lookup. A second read is a
	// second answer, and a unit that runs as one identity while being
	// authorized as another is the gap this shares one read to close.
	if err := e.authorizeUnit(stmt, pol); err != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, err)
	}
	if err := e.profileFor(connRow).admit(stmt, true); err != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, err)
	}
	if err := guardWhere(stmt); err != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, err)
	}

	s.mu.Lock()
	pinned, phase, txID := s.tx, s.txPhase, s.txID
	s.mu.Unlock()
	if phase == txAborted {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAborted)
	}

	runCtx, endRun := s.runContext(ctx)
	defer endRun()
	res, rerr := e.executeUnit(runCtx, execUnit{
		stmt: stmt, pol: pol, connRow: connRow, sqlText: sqlText, ip: ip,
		pinned: pinned, txID: txID,
	})
	s.noteStatementOutcome(rerr)
	return res, rerr
}

// wireControl routes a transaction verb on a PAT-backed wire session.
func (e *Engine) wireControl(
	ctx context.Context, s *session, connRow *meta.Connection,
	stmt Statement, pol UnitPolicy, sqlText, ip string,
) (*Result, error) {
	if err := e.profileFor(connRow).admit(stmt, true); err != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, err)
	}
	// The floor follows the policy, exactly as it does on the token path: a
	// unit that will run read-only needs the read floor, and anything that
	// could write still needs the write one. Requiring write unconditionally
	// is what made the read-only wrap unreachable for explicit transactions.
	if !pol.MayWrite && !pol.ReadOnly {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, auth.ErrDenied)
	}

	// LOCK is real SQL, not a transaction verb, and PostgreSQL permits it in
	// read-only transactions. The token path re-enters run and gets this floor
	// from classToAction; this wire-only stateful route bypasses run, so it must
	// enforce the equivalent boundary here.
	if stmt.Verb == "LOCK" && !pol.MayWrite {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, auth.ErrDenied)
	}

	// SET and LOCK are real SQL that must reach the server; the transaction
	// verbs never do.
	if statefulControlVerbs[stmt.Verb] {
		s.mu.Lock()
		txOpen, aborted, pinned, txID := s.txPhase != txNone, s.txPhase == txAborted, s.tx, s.txID
		s.mu.Unlock()
		if aborted {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAborted)
		}
		if err := e.admitSessionState(ctx, s, pol.Ident, stmt.Verb, sqlText, ip, txOpen); err != nil {
			return nil, err
		}
		runCtx, endRun := s.runContext(ctx)
		defer endRun()
		return e.executeUnit(runCtx, execUnit{
			stmt: stmt, pol: pol, connRow: connRow, sqlText: sqlText, ip: ip,
			pinned: pinned, txID: txID,
		})
	}

	tc, perr := ParseTxControl(sqlText)
	if perr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
	}
	return e.handleTxControl(ctx, s, pol, connRow, tc, sqlText, ip)
}

// authorizeUnit decides a statement's class against an ALREADY-RESOLVED
// policy.
//
// The same rule auth.decide applies, asked of a verdict that has already been
// read rather than by reading again. Read is the floor for standing at all —
// a policy that exists means the caller cleared it — so what is left to decide
// is whether the statement needs more than that.
func (e *Engine) authorizeUnit(stmt Statement, pol UnitPolicy) error {
	switch classToAction(stmt.Class) {
	case auth.ActionRead:
		return nil // standing IS the read floor
	case auth.ActionWrite, auth.ActionDDL:
		if !pol.MayWrite {
			return auth.ErrDenied
		}
		return nil
	default:
		// Manage is not reachable from a statement class, and a class this
		// function does not recognise must not be waved through: an action
		// nobody mapped is an action nobody authorized.
		return auth.ErrDenied
	}
}

// execUnit is one unit's inputs, so the shared executor's signature does not
// grow a parameter per caller.
type execUnit struct {
	stmt    Statement
	pol     UnitPolicy
	connRow *meta.Connection
	sqlText string
	ip      string
	pinned  dao.TxConn
	txID    string
}

// executeUnit is the shared tail: attempt record, read-only wrap, target,
// execution.
//
// Everything after resolution, in one place, so the wire path and the token
// path cannot drift about what running a statement means.
func (e *Engine) executeUnit(ctx context.Context, u execUnit) (*Result, error) {
	var target dao.DataConn
	if u.pinned == nil {
		t, err := e.target(ctx, u.connRow.ID, u.connRow)
		if err != nil {
			e.auditBounded(ctx, u.pol.Ident.UserID(), u.ip, "exec_conn_failed",
				fmt.Sprintf("conn %d: %v", u.connRow.ID, err))
			return nil, err
		}
		target = t
	}

	attemptID, err := e.recordAttempt(ctx, u.pol.Ident, u.connRow.ID, u.ip, u.sqlText, u.txID)
	if err != nil {
		return nil, err
	}

	pinned := u.pinned
	if pinned == nil && u.pol.ReadOnly {
		wrapped, release, werr := e.wrapReadOnly(ctx, target, u.connRow, u.pol.Ident,
			u.connRow.ID, u.ip, u.sqlText, u.pol)
		if werr != nil {
			return nil, werr
		}
		if release != nil {
			defer release()
		}
		if wrapped != nil {
			pinned = wrapped
		}
	}

	res := &Result{Verb: u.stmt.Verb, Class: u.stmt.Class}
	start := e.now()
	var runErr error
	var rowCount int64
	switch {
	case pinned != nil && u.stmt.Class == ClassRead:
		rowCount, runErr = e.queryOn(ctx, pinned, u.sqlText, res, nil)
	case pinned != nil:
		runErr = e.execOn(ctx, pinned, u.sqlText, res)
		rowCount = res.Affected
	case u.stmt.Class == ClassRead:
		rowCount, runErr = e.runQuery(ctx, target, u.connRow.Engine, u.sqlText, res, nil)
	default:
		runErr = e.runExec(ctx, target, u.connRow.Engine, u.sqlText, res)
		rowCount = res.Affected
	}
	res.Duration = e.now().Sub(start)

	// The outcome append runs on an internal bounded context so a cancelled
	// caller cannot suppress the record — the same reason the token path
	// does it, and the same code would be better still, but the two differ
	// only in this tail and sharing it would mean threading onRow and a
	// token through a struct for one line. Kept identical deliberately; if
	// it grows, it moves.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := e.recordOutcome(recCtx, u.pol.Ident, u.connRow.ID, u.ip, attemptID,
		res.Duration, rowCount, runErr, u.txID); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, fmt.Errorf("exec: statement failed: %w", runErr)
	}
	return res, nil
}
