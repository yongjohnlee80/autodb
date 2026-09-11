package exec

import (
	"context"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/admission"
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
// executors, the same read-only wrap. That is the seam the design
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
	release, closeAfterRelease, err := e.claimSession(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()
	return e.wireExecuteClaimed(ctx, s, sqlText, ip, closeAfterRelease)
}

// wireExecuteClaimed is WireExecute AFTER the session claim: the caller holds
// s.begin() and owns s.finish(). It exists so WireQuery can keep ONE claim
// across gate, dispatch, every emit, and the status read (found in
// review) — a WireQuery built on WireExecute released the claim in
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
	if connRow, cerr := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get(); cerr == nil && connRow.Engine.SpeaksPostgresWire() {
		if _, perr := e.pinWireSession(ctx, s, connRow); perr != nil {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
		}
	}
	return e.executeSessionUnit(ctx, s, pol, sqlText, ip, true, closeAfterRelease)
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
	admitErr, opErr := e.runWireGrammarAdmission(s)
	if opErr != nil {
		return UnitPolicy{}, opErr
	}
	if admitErr != nil {
		return UnitPolicy{}, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}
	return pol, nil
}

// executeSessionUnit is the shared token/PAT session pipeline after one fresh
// policy has been resolved and transaction authority has been preflighted.
// Control authorization remains surface-owned because its two independent
// gates are load-bearing P2 evidence; ordinary execution shares everything.
func (e *Engine) executeSessionUnit(
	ctx context.Context, s *session, pol UnitPolicy, sqlText, ip string, wire bool, closeAfterRelease *bool,
) (*Result, error) {

	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if err != nil {
		return nil, auth.ErrDenied // never disclose which connections exist
	}

	// Reject oversized input BEFORE classification or control routing, for the
	// same reason the token path does:
	// the audit record must equal exactly what ran, and an unaudited tail must
	// never execute. The wire path omitted this, so a statement of any size
	// reached the classifier here while the identical statement was refused on
	// the token path.
	//
	// Placed above Classify so it also covers CONTROL statements: wireControl
	// is reached only through the routing below, so a gate here is the single
	// point that governs both.
	phys := admission.PhysSession
	if wire {
		phys = admission.PhysWire
	}
	admitErr, opErr := e.runSizeAdmission(phys, sqlText)
	if opErr != nil {
		return nil, opErr
	}
	if admitErr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}

	stmt, cerr := Classify(sqlText, connRow.Engine.BackslashEscapes())
	if cerr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, cerr)
	}

	// Transaction control is a state transition, routed before the execution
	// pipeline is entered at all — the same routing the token path does, for
	// the same reason.
	if stmt.Class == ClassControl {
		if wire {
			return e.wireControl(ctx, s, connRow, stmt, pol, sqlText, ip, closeAfterRelease)
		}
		return e.tokenControl(ctx, s, connRow, stmt, pol, sqlText, ip, closeAfterRelease)
	}

	// THE STATEMENT GATES, through the one chain. The legacy session path
	// ran reader analysis and class authorization before the profile gate;
	// the profile runs first now. The gate matrix names both resulting
	// identity changes and records why neither can reach the corpus manifest.
	// A compat dm-CTE with a UDF previously answered reader-advanced-pattern;
	// the same shape without a UDF previously answered auth.ErrDenied.
	// Profile-first answers statement-unsupported in both cases, and named
	// cells keep this accepted disclosure change separate from preservation.
	s.mu.Lock()
	pinned, phase, txID := s.tx, s.txPhase, s.txID
	s.mu.Unlock()
	admitErr, opErr = e.runSessionAdmission(ctx, s, pol, connRow, phase != txNone, stmt, sqlText)
	if opErr != nil {
		return nil, opErr
	}
	if admitErr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}
	if phase == txAborted {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAborted)
	}

	runCtx, endRun := s.runContext(ctx)
	defer endRun()
	res, rerr := e.executeUnit(runCtx, execUnit{
		stmt: stmt, pol: pol, connRow: connRow, sqlText: sqlText, ip: ip,
		pinned: pinned, txID: txID, tag: s.auditTag(), phys: phys,
	})
	s.noteStatementOutcome(rerr)
	return res, rerr
}

// wireControl routes a transaction verb on a PAT-backed wire session.
func (e *Engine) wireControl(
	ctx context.Context, s *session, connRow *meta.Connection,
	stmt Statement, pol UnitPolicy, sqlText, ip string, closeAfterRelease *bool,
) (*Result, error) {
	admitErr, opErr := e.runProfileAdmission(e.profileFor(connRow), admission.PhysWire, s.pinnedConn() != nil, stmt, sqlText)
	if opErr != nil {
		return nil, opErr
	}
	if admitErr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
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

	// DO and CALL are real SQL the target runs, dispatched as text. They take
	// the ordinary execution unit, and they clear the gates a dispatched
	// statement clears — the floors above are the control route's, and those
	// admit a reader, which is right for BEGIN and wrong for a procedural body.
	if proceduralControlVerbs[stmt.Verb] {
		s.mu.Lock()
		txOpen, aborted, pinned, txID := s.txPhase != txNone, s.txPhase == txAborted, s.tx, s.txID
		s.mu.Unlock()
		admitErr, opErr = e.runProceduralAdmission(ctx, s, pol, connRow, txOpen, stmt, sqlText)
		if opErr != nil {
			return nil, opErr
		}
		if admitErr != nil {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
		}
		if aborted {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAborted)
		}
		runCtx, endRun := s.runContext(ctx)
		defer endRun()
		res, rerr := e.executeUnit(runCtx, execUnit{
			stmt: stmt, pol: pol, connRow: connRow, sqlText: sqlText, ip: ip,
			pinned: pinned, txID: txID, tag: s.auditTag(), phys: admission.PhysWire,
		})
		if rerr == nil {
			// The body is opaque, so a routine may have been defined or
			// dropped inside it. Class-based invalidation cannot see that —
			// DO is control, not DDL — and a stale routine set is how a
			// reader reaches a function created a moment ago.
			e.invalidateRoutines(connRow.ID)
		}
		return res, rerr
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
		if err := e.admitSessionState(ctx, s, pol, stmt, sqlText, ip, txOpen); err != nil {
			return nil, err
		}
		runCtx, endRun := s.runContext(ctx)
		defer endRun()
		return e.executeUnit(runCtx, execUnit{
			stmt: stmt, pol: pol, connRow: connRow, sqlText: sqlText, ip: ip,
			pinned: pinned, txID: txID, tag: s.auditTag(), phys: admission.PhysWire,
		})
	}

	tc, perr := ParseTxControl(sqlText)
	if perr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
	}
	return e.handleTxControl(ctx, s, pol, connRow, tc, sqlText, ip, closeAfterRelease)
}

// authorizeUnit decides a statement's class against an ALREADY-RESOLVED
// policy.
//
// The same rule auth.decide applies, asked of a verdict that has already been
// read rather than by reading again. Read is the floor for standing at all —
// a policy that exists means the caller cleared it — so what is left to decide
// is whether the statement needs more than that.
func authorizeUnit(stmt Statement, pol UnitPolicy) error {
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
	tag     string // session stamp for audit lines; empty on the token path
	phys    admission.PhysicalCtx
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

	attemptID, err := e.recordAttemptTagged(ctx, u.pol.Ident, u.connRow.ID, u.ip, u.sqlText, u.txID, u.tag)
	if err != nil {
		return nil, err
	}

	pinned := u.pinned
	if pinned == nil && u.pol.ReadOnly {
		wrapped, release, werr := e.wrapReadOnly(ctx, target, u.pol.Ident,
			u.connRow.ID, u.ip, u.sqlText, u.pol, u.phys)
		if werr != nil {
			return nil, e.rejectRecordedAttempt(ctx, u.pol.Ident, u.connRow.ID, u.ip, u.sqlText, attemptID, werr)
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
	status, errText := StatusOK, ""
	switch {
	case runErr != nil:
		status, errText = StatusError, truncate(runErr.Error(), maxErrorBytes)
	case u.txID != "":
		status = StatusPendingCommit
	}
	if err := e.writeOutcomeTagged(recCtx, u.pol.Ident, u.connRow.ID, u.ip, attemptID,
		res.Duration, rowCount, status, errText, u.txID, u.tag); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, fmt.Errorf("exec: statement failed: %w", runErr)
	}
	if u.stmt.Class == ClassDDL {
		e.invalidateRoutines(u.connRow.ID) // a routine may have been defined or dropped
	}
	return res, nil
}
