package exec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// The session-scoped engine API.
//
// Authority is NEVER cached from OpenSession. Every call below re-resolves the
// token and re-authorizes at the statement's own class, exactly as the
// stateless path does — a session is a place to keep a transaction, never a
// place to keep a permission. Ownership is re-checked on every call too, so a
// grant removed between two statements takes effect on the second one.

// OpenSession creates a session bound to one connection for one user.
func (e *Engine) OpenSession(ctx context.Context, token string, connID int64, ip string) (SessionID, error) {
	s, err := e.openSession(ctx, token, connID, ip)
	if err != nil {
		return "", err
	}
	return s.id, nil
}

// openSession is OpenSession returning the session OBJECT.
//
// An engine-internal caller that opens a session must hold the thing it
// opened, not just its id. Going back through the public API to close it
// means re-authenticating a token that may no longer be valid — and the
// engine already knows this session is its own, so asking permission to
// clean up after itself is both unnecessary and unsafe.
func (e *Engine) openSession(ctx context.Context, token string, connID int64, ip string) (*session, error) {
	ident, authSessID, err := e.auth.SessionRef(ctx, token)
	if err != nil {
		return nil, err
	}
	// Read is the floor for holding a session at all, and it is checked
	// BEFORE the connection row is read — an ungranted caller must not learn
	// whether a connection exists (the same rule the stateless path follows).
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return nil, e.reject(ctx, ident, connID, ip, "", err)
	}
	if _, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get(); err != nil {
		return nil, e.reject(ctx, ident, connID, ip, "", auth.ErrDenied)
	}

	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	// The session's context deliberately does NOT inherit the caller's
	// cancellation: this RPC call is about to return, and the session has to
	// outlive it. It keeps the values (tracing, deadline-free) and gets its
	// own cancel, which is what a close uses to stop in-flight work.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		id:        id,
		userID:    ident.UserID(),
		authority: auth.SessionAuthority(authSessID),
		connID:    connID,
		ctx:       sctx,
		cancel:    cancel,
		lastUsed:  e.now(),
		// A token session holds a backend for as long as its transaction
		// lives, so it is a holder too and the heartbeat covers it. It has no
		// PAT and no startup username: those render as explicit nulls rather
		// than being invented.
		holderIP:   ip,
		acquiredAt: e.now(),
	}
	if err := e.sessions.admit(s); err != nil {
		cancel()
		// Cap refusals are audited: a caller hitting a limit repeatedly is
		// either misbehaving or under-provisioned, and neither is visible
		// without a record.
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "session_refused",
			fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
			return nil, aerr
		}
		return nil, err
	}
	if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "session_opened",
		fmt.Sprintf("conn %d: session %s", connID, id)); aerr != nil {
		e.closeSession(context.WithoutCancel(ctx), s, ip, "audit-failed")
		return nil, aerr
	}
	return s, nil
}

// CloseSession closes a session the caller owns.
func (e *Engine) CloseSession(ctx context.Context, token string, id SessionID, ip string) error {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return err
	}
	s, err := e.sessions.lookup(id, ident.UserID())
	if err != nil {
		return err
	}
	e.closeSession(ctx, s, ip, "client-closed")
	return nil
}

// SessionExecute runs one statement on a session.
//
// Everything the stateless path does still happens — token, grant floor,
// classification, profile admission, guard, audit — because a session changes
// WHERE a statement runs, never WHETHER it is allowed to.
func (e *Engine) SessionExecute(ctx context.Context, token string, id SessionID, sqlText, ip string) (*Result, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	s, err := e.sessions.lookup(id, ident.UserID())
	if err != nil {
		return nil, err
	}
	release, closeAfterRelease, err := e.claimSession(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// Re-check the state after claiming the slot. A close that began between
	// the lookup and the claim has already cancelled the session context, and
	// running a statement into it would be work nobody can observe the end of.
	if h := e.sessions.hookAfterStateCheck; h != nil {
		h()
	}
	if s.get() != sessOpen {
		return nil, ErrSessionNotFound
	}

	// Resolve authority ONCE after the slot/state check and carry that exact
	// answer through preflight, control routing, authorization and execution.
	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, perr)
	}
	demoted, derr := e.enforceTransactionAuthority(ctx, s, pol, ip)
	if derr != nil {
		// Own closing before the slot is released. If a concurrent closer
		// already owns it, that closer is waiting on this same slot and will
		// resume after the defer above releases it.
		*closeAfterRelease = e.transferDemotionClose(s, ip)
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText,
			fmt.Errorf("%w: rollback cleanup failed: %v", ErrTxAuthorityChanged, derr))
	}
	if demoted {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, ErrTxAuthorityChanged)
	}
	return e.executeSessionUnit(ctx, s, pol, sqlText, ip, false, closeAfterRelease)
}

// tokenControl preserves the token path's ClassControl authorization floor.
// The wire route has its own explicit LOCK gate because it does not re-enter
// the token pipeline; keeping the two boundaries distinct makes each surface's
// mutation cell independently discriminating.
func (e *Engine) tokenControl(
	ctx context.Context, s *session, connRow *meta.Connection,
	stmt Statement, pol UnitPolicy, sqlText, ip string, closeAfterRelease *bool,
) (*Result, error) {
	admitErr, opErr := e.runProfileAdmission(e.profileFor(connRow), admission.PhysSession, false, stmt, sqlText)
	if opErr != nil {
		return nil, opErr
	}
	if admitErr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}

	if statefulControlVerbs[stmt.Verb] {
		admitErr, opErr = e.runClassAdmission(pol, admission.PhysSession, stmt, sqlText)
		if opErr != nil {
			return nil, opErr
		}
		if admitErr != nil {
			return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
		}
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
			pinned: pinned, txID: txID, phys: admission.PhysSession, s: s,
		})
	}

	tc, perr := ParseTxControl(sqlText)
	if perr != nil {
		return nil, e.rejectSession(ctx, s, pol.Ident, ip, sqlText, perr)
	}
	return e.handleTxControl(ctx, s, pol, connRow, tc, sqlText, ip, closeAfterRelease)
}

// admitSessionState applies the orchestrated stateful gate and preserves the
// drive-owned administrative floor for pooled/session SET.
//
// The role floor is here rather than in the gate because it is a policy
// question, not a grammar one: SET LOCAL is admin-only by default per
// The design says so, and LOCK takes the write floor already checked above. The
// sub-capability grant that would let an operator delegate SET LOCAL more
// finely is not built yet, so the default stands alone for now — which is
// the restrictive direction.
func (e *Engine) admitSessionState(
	ctx context.Context, s *session, pol UnitPolicy,
	stmt Statement, sqlText, ip string, txOpen bool,
) error {
	s.mu.Lock()
	wire := s.wire
	s.mu.Unlock()
	phys := admission.PhysSession
	if wire {
		phys = admission.PhysWire
	}
	admitErr, opErr := e.runSessionStateAdmission(pol, phys, txOpen, stmt, sqlText)
	if opErr != nil {
		return opErr
	}
	if admitErr != nil {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText, admitErr)
	}
	if !wire && stmt.Verb == "SET" && pol.Ident.Role() != meta.RoleAdmin {
		return e.rejectSession(ctx, s, pol.Ident, ip, sqlText,
			fmt.Errorf("%w: SET LOCAL is admin-only by default", auth.ErrDenied))
	}
	return nil
}

// rejectSession audits a refusal on a session-scoped call and returns it.
func (e *Engine) rejectSession(ctx context.Context, s *session, ident auth.Identity, ip, sqlText string, cause error) error {
	detail := fmt.Sprintf("conn %d: session %s: %v: %s", s.connID, s.id, cause, truncate(sqlText, maxAuditSQLBytes))
	if err := e.auth.Audit(ctx, ident.UserID(), ip, "exec_rejected", detail); err != nil {
		return err
	}
	return cause
}

// closeSession performs the terminal transition exactly once.
//
// The CAS decides the initial owner and closeActive excludes every retry while
// that owner is running. An owner that cannot quiesce explicitly releases only
// finalizer ownership, not the closing state, so a later janitor pass can retry
// without ever running two finalizers concurrently.
func (e *Engine) closeSession(ctx context.Context, s *session, ip, reason string) {
	if !s.beginClose(ip, reason) {
		return // someone else owns the teardown
	}
	e.finishClosing(ctx, s)
}

// retryClose resumes a close that could not finish because the session's
// statement would not stop. The session is already in the closing state and
// still owns its transaction; this is the retry that eventually ends it.
func (e *Engine) retryClose(ctx context.Context, s *session) bool {
	if !s.claimCloseRetry() {
		return false
	}
	e.finishClosing(ctx, s)
	return true
}

func (e *Engine) finishClosing(ctx context.Context, s *session) {
	// No registry lock is held here: quiescing can block on a statement, and
	// the published lock order forbids waiting on session I/O under the
	// registry's mutex.
	//
	// The result of the join is CHECKED. It was not before: the close waited
	// ten seconds and then rolled back whichever way the wait had gone, so a
	// statement still executing would receive a rollback on its own
	// connection — the same concurrent-command bug as the timeout path, in
	// the path that runs on every ordinary close.
	release, quiesced := e.quiesce(ctx, s, e.closeQuiesce)
	defer release()

	s.mu.Lock()
	ip, reason := s.closeIP, s.closeWhy
	tx, txID := s.tx, s.txID
	if quiesced == nil {
		// Detach only when it is safe to. Leaving the transaction attached
		// to a session that is going away is the lesser evil: the pool's
		// teardown and the server-side belt both end it, whereas rolling
		// back underneath a live statement corrupts the connection state for
		// whatever the pool hands out next.
		s.clearTxLocked()
	}
	s.mu.Unlock()

	if quiesced != nil && tx != nil {
		// The statement would not stop, so the transaction cannot be rolled
		// back without racing it. The session therefore STAYS — in the
		// registry, in the closing state, still owning its transaction.
		//
		// Removing it here is what the previous version did, and that was
		// the worse half of the bug: the rollback was correctly skipped, and
		// then the only object that knew about the live transaction was
		// dropped. Nothing could retry it, nothing could account for it, and
		// conn.delete's pool close would wait on a connection no reachable
		// owner held. Retaining a closing owner is what makes the skip
		// recoverable instead of terminal — reapExpired retries it, and the
		// session keeps counting against the caps because it is still
		// holding a connection.
		e.logf("session %s: closing on %s but the in-flight statement would not stop (%v); "+
			"keeping the session in closing state so the transaction stays owned and the "+
			"janitor can retry", s.id, reason, quiesced)
		e.auditBounded(ctx, s.userID, ip, "tx_rollback_deferred",
			fmt.Sprintf("conn %d: session %s: %s: %s: in-flight statement did not stop; retained for retry",
				s.connID, s.id, txID, reason))
		// Same reasoning as the timeout sweep: the rollback was DECIDED
		// against rather than failed, the transaction is live and owned, and
		// the janitor will retry. Undetermined is what the log should say
		// until it is not.
		e.noteTxOutcome(ctx, txTransition{
			txID: txID, state: meta.TxUnknownPending, reason: meta.ReasonSessionClosed,
			userID: s.userID, connectionID: s.connID,
		})
		if s.releaseCloseForRetry() {
			// A demotion cleanup failure arrived while this owner was
			// recording its deferral. Keep ownership continuous and retry on
			// the overriding reason rather than waiting for another sweep.
			e.finishClosing(context.WithoutCancel(ctx), s)
		}
		return
	}
	// txResidue is what the release gate cannot see for itself: whether this
	// session's transaction really ended. Only the code that ran the rollback
	// knows, and a gate that assumed success would return a backend still
	// holding the transaction's locks to the pool.
	var txResidue error
	if tx != nil {
		// A FRESH bounded context: the session's own is cancelled by now, and
		// a rollback that cannot run because its context is gone would leave
		// a transaction holding locks on a live database with nobody left to
		// close it.
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
		rerr := tx.RollbackContext(cctx)
		ccancel()
		outcome := FinalizeRolledBack
		if rerr != nil {
			outcome = "rollback_failed"
			txResidue = rerr
			e.logf("session %s: rolling back %s on close: %v", s.id, txID, rerr)
		}
		e.noteTxOutcome(ctx, txTransition{
			txID: txID, state: txStateFor(outcome, rerr), reason: txOutcomeReason(outcome, rerr),
			userID: s.userID, connectionID: s.connID,
		})
		e.auditBounded(ctx, s.userID, ip, "tx_"+string(outcome),
			fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, reason))
	}

	// The pinned backend outlives the session ONLY IF a reset proves it carries
	// nothing forward. The wire carried this client's settings, its prepared
	// names, possibly its temporary tables and an advisory lock or two, and
	// possibly a poisoned face; release_gate.go runs the reset, checks the
	// connection reports itself idle afterwards, and hands it back to the pool
	// only on complete success. Every other ending closes the socket. That
	// decision is not taken here, so that there is exactly one of it.
	s.mu.Lock()
	pc := s.pc
	s.pc = nil
	s.mu.Unlock()
	if pc != nil {
		// A context WITHOUT the caller's cancellation, for the same reason the
		// rollback above needs one: a reset that cannot run because its context
		// is already gone would discard every backend on a shutdown path, and
		// worse, would make the gate's verdict a property of the caller's
		// deadline rather than of the backend.
		verdict := e.releaseBackend(context.WithoutCancel(ctx), s, pc, txResidue)
		e.noteBackendFate(ctx, s, ip, verdict)
	}
	// The session's own context is cancelled last, once nothing is running on
	// it and the rollback has had its fresh context.
	s.cancel()

	e.sessions.remove(s)
	s.finishClose()

	e.auditBounded(ctx, s.userID, ip, "session_closed",
		fmt.Sprintf("conn %d: session %s: %s", s.connID, s.id, reason))
}

// transferDemotionClose publishes the cleanup-failure reason while the caller
// still owns the execution slot. If an ordinary closer already owns the state
// transition, its finalizer reads this overriding reason only after it acquires
// the slot, so it cannot report a client close for a demotion cleanup failure.
func (e *Engine) transferDemotionClose(s *session, ip string) bool {
	owner := s.transferClose(ip, reasonDemotionCleanupFailed)
	if h := e.hookDemotionCloseOwned; h != nil {
		h()
	}
	return owner
}

// closeQuiesceTimeout bounds the wait for an in-flight statement during a
// close. Longer than the timeout path's: a close is usually a person asking,
// and finishing the rollback properly is worth a few more seconds.
const closeQuiesceTimeout = 15 * time.Second

// closeSessionsFor closes every session on a connection. It is the first step
// of deleting or closing a connection: the pool must not go away underneath a
// session that still believes it can run.
func (e *Engine) closeSessionsFor(ctx context.Context, connID int64, ip, reason string) {
	drained, pool := e.beginDraining(connID)
	for _, s := range drained {
		e.closeSession(ctx, s, ip, reason)
	}
	// The pool goes last, and with a bound. Its Close waits for every
	// acquired connection, so a session whose statement would not stop —
	// the case closeSession audits rather than rolling back — would
	// otherwise hang conn.delete indefinitely. The wait is worth having,
	// because a clean Close returns the connections to the target; the
	// bound is worth having because an operator's delete must return.
	if pool != nil {
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			_ = pool.Close()
		}()
		select {
		case <-closed:
		case <-time.After(poolCloseTimeout):
			e.logf("connection %d: the pool did not close within %s (a statement is still "+
				"holding one of its connections); abandoning it to the driver", connID, poolCloseTimeout)
		}
	}
}

// poolCloseTimeout bounds how long conn.delete waits for a target pool to
// shut down cleanly.
const poolCloseTimeout = 20 * time.Second

// CloseAllSessions closes every open session (engine shutdown).
func (e *Engine) CloseAllSessions(ctx context.Context, reason string) {
	for _, s := range e.sessions.snapshot() {
		e.closeSession(ctx, s, "", reason)
	}
}

// reapIdleSessions IS DELETED, and this note is here so nobody re-adds it.
//
// It held the idle rung and R7, and read exactly like the sweep the janitor
// runs — but StartJanitor calls reapExpired, and reapIdleSessions had NO
// production caller at all. R7 therefore could not fire in a running daemon,
// while its cells passed and the milestone read as delivered. Both rungs now
// live in reapExpired (session_timeout.go), which is the one the janitor
// drives, and the cells drive that.
//
// The lesson is the reason this comment exists rather than a silent deletion:
// a second reaper that resembles the real one is indistinguishable from the
// real one in a test, and the resemblance is what hid the defect.

// ReasonR7DependencyTimeout is the audit identity for a session ended because
// what it was holding stopped moving.
const ReasonR7DependencyTimeout = "r7-dependency-timeout"

// r7DependencyBound is how long held objects may go without progress.
//
// TWO HOURS, AND IT IS A BOUND ON THE DEPENDENCY RATHER THAN ON THE WIRE. A
// client legitimately holding a prepared statement across a quiet period is
// doing nothing wrong; one that has not touched it in two hours while chatting
// continuously has abandoned it in every sense except the protocol's.
const r7DependencyBound = 2 * time.Hour

// r7Expired reports whether this session has reached R7's bound.
//
// EVERY CONDITION IS REQUIRED, and each excludes somebody who would be harmed:
//
//   - nothing held -> it pins nothing, and the idle rung already owns it;
//   - a request in flight -> ending it cancels work inside its bounds;
//   - a transaction open -> ending it rolls back work nobody abandoned;
//   - wire-idle past the idle bound -> that is the rung above, not this one,
//     and letting both match would make which reason is recorded depend on
//     evaluation order rather than on what happened.
func (e *Engine) r7Expired(s *session, now time.Time, idle time.Duration) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || s.tx != nil || s.ext == nil || !s.ext.holdsAnything() {
		return false
	}
	// COMPUTED HERE RATHER THAN THROUGH idleFor, which takes this same mutex.
	// Calling it from inside the hold would deadlock the janitor sweep against
	// itself -- found by the compiler refusing an unrelated line, not by the
	// deadlock, which would have appeared in production as a daemon that
	// stopped reaping and never said why.
	if now.Sub(s.lastUsed) >= idle {
		return false
	}
	return s.ext.dependencyIdleFor(now) >= r7DependencyBound
}

// logf reports an operational problem that has no caller to return to.
func (e *Engine) logf(format string, args ...any) {
	if e.onLog != nil {
		e.onLog(fmt.Sprintf(format, args...))
	}
}

// compile-time proof the session path uses the same dao surface as the rest.
var _ = dao.ErrNoRows
var _ = errors.Is

// claimSession takes the session's ONE in-flight slot and returns the release
// that gives it back.
//
// THE THREE COPIES THIS REPLACES WERE IDENTICAL AND SAFETY-CRITICAL, which is a
// bad combination. session_engine.go, wire_execute.go and wire_query.go each
// wrote out s.begin(), a closeAfterRelease flag, and a defer that calls
// s.finish() and then conditionally finishClosing. Nothing tied them together,
// and the thing that would diverge silently is ORDER: finish() must run before
// finishClosing, because finishClosing tears down a session that must no longer
// be claimed, and it must run under a context the caller's cancellation cannot
// reach — a close that begins as the statement ends would otherwise be
// abandoned halfway.
//
// The flag is returned as a POINTER because the decision to close is made later
// and deeper: wireAdmit and transferDemotionClose set it after the claim, from
// inside the work the claim protects. The caller holds the defer, so the caller
// must hold the flag.
//
//	release, closeAfterRelease, err := e.claimSession(ctx, s)
//	if err != nil {
//	    return <zero>, err
//	}
//	defer release()
//
// The zero value is why this returns a release instead of taking the defer
// itself: the three callers return different types, and a helper that returned
// early for them would have to know which.
func (e *Engine) claimSession(ctx context.Context, s *session) (release func(), closeAfterRelease *bool, err error) {
	// One in-flight statement per session. Claimed before any work so a second
	// caller is refused rather than queued behind work it cannot see.
	if err := s.begin(); err != nil {
		return nil, nil, err
	}
	flag := false
	return func() {
		s.finish()
		if flag {
			e.finishClosing(context.WithoutCancel(ctx), s)
		}
	}, &flag, nil
}

// SessionsInTransaction reports how many sessions currently hold an open
// transaction.
//
// EXPORTED FOR THE SHUTDOWN DECISION. Stopping the server severs every open
// transaction: the connections drop, the targets roll them back, and the work
// a caller has not committed is gone. The drain does not cover this — it
// cancels in-flight handler contexts and waits for them to unwind, so a
// session parked BETWEEN statements inside a transaction has no in-flight
// handler for the drain to see at all.
//
// A count rather than a boolean, because the caller has to tell an operator
// HOW MANY sessions they would be interrupting; "something is open" is not
// something anyone can act on.
func (e *Engine) SessionsInTransaction() int { return e.sessions.countInTransaction() }

// SessionsExecuting reports how many sessions are running a statement now.
//
// EXPORTED FOR THE RESTART PROMPT, and it is deliberately not
// SessionsInTransaction. The shutdown drain CANCELS in-flight handler contexts,
// so the work a restart destroys without asking is the statements currently
// running -- which is what an operator has to be told. Open transactions are a
// different population and are already handled by a different mechanism:
// BeginShutdown REFUSES to stop while any are open, so they are not silently
// lost and the prompt must not present them as though they were.
//
// A READING, NOT A GUARANTEE. Sessions start and finish statements without
// asking anyone, so this is true when it is taken and may be false a moment
// later. It exists to inform a decision, not to make one: nothing may gate a
// shutdown on it, because the gate that makes shutdown safe is BeginShutdown's
// single atomic step, and a second check would only add a window.
func (e *Engine) SessionsExecuting() int { return e.sessions.countExecuting() }

// BeginShutdown is the shutdown decision, taken as ONE step: it closes
// transaction admission and reports how many transactions were open at the
// instant it closed.
//
// Zero means admission STAYS closed and the caller may commit the shutdown.
// Non-zero means nothing was closed and the caller must not stop. A caller that
// got zero and then decided not to stop after all must call AbortShutdown, or
// the daemon keeps refusing to begin transactions it is never going to end.
//
// Separate from SessionsInTransaction because that one only READS. Reading and
// then acting are two steps with a window between them, and a BEGIN admitted
// in that window was torn down by the drain -- which is the loss the refusal
// exists to prevent.
func (e *Engine) BeginShutdown() (blockers int, token uint64) {
	return e.sessions.closeTxAdmission()
}

// AbortShutdown reopens transaction admission after a shutdown that was
// decided on but not carried out.
func (e *Engine) AbortShutdown(token uint64) bool { return e.sessions.reopenTxAdmission(token) }
