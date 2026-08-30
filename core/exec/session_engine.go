package exec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// The session-scoped engine API (ADR-0074 §1, §1b).
//
// Authority is NEVER cached from OpenSession. Every call below re-resolves the
// token and re-authorizes at the statement's own class, exactly as the
// stateless path does — a session is a place to keep a transaction, never a
// place to keep a permission. Ownership is re-checked on every call too, so a
// grant removed between two statements takes effect on the second one.

// OpenSession creates a session bound to one connection for one user.
func (e *Engine) OpenSession(ctx context.Context, token string, connID int64, ip string) (SessionID, error) {
	ident, authSessID, err := e.auth.SessionRef(ctx, token)
	if err != nil {
		return "", err
	}
	// Read is the floor for holding a session at all, and it is checked
	// BEFORE the connection row is read — an ungranted caller must not learn
	// whether a connection exists (the same rule the stateless path follows).
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return "", e.reject(ctx, ident, connID, ip, "", err)
	}
	if _, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get(); err != nil {
		return "", e.reject(ctx, ident, connID, ip, "", auth.ErrDenied)
	}

	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	// The session's context deliberately does NOT inherit the caller's
	// cancellation: this RPC call is about to return, and the session has to
	// outlive it. It keeps the values (tracing, deadline-free) and gets its
	// own cancel, which is what a close uses to stop in-flight work.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		id:         id,
		userID:     ident.UserID(),
		authSessID: authSessID,
		connID:     connID,
		ctx:        sctx,
		cancel:     cancel,
		lastUsed:   e.now(),
	}
	if err := e.sessions.admit(s); err != nil {
		cancel()
		// Cap refusals are audited: a caller hitting a limit repeatedly is
		// either misbehaving or under-provisioned, and neither is visible
		// without a record.
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "session_refused",
			fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
			return "", aerr
		}
		return "", err
	}
	if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "session_opened",
		fmt.Sprintf("conn %d: session %s", connID, id)); aerr != nil {
		e.closeSession(context.WithoutCancel(ctx), s, ip, "audit-failed")
		return "", aerr
	}
	return id, nil
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
	// One in-flight statement per session. Claimed before any work so a
	// second caller is refused rather than queued behind work it cannot see.
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.finish()

	// Re-check the state after claiming the slot. A close that began between
	// the lookup and the claim has already cancelled the session context, and
	// running a statement into it would be work nobody can observe the end of.
	if h := e.sessions.hookAfterStateCheck; h != nil {
		h()
	}
	if s.get() != sessOpen {
		return nil, ErrSessionNotFound
	}

	// A control verb is a state transition, not SQL to forward, so it is
	// routed before the execution pipeline is entered at all (ADR-0074 §3).
	// Everything the pipeline would have done to it — classify, admit,
	// authorize — still happens; it just ends in a transition rather than a
	// statement on the wire.
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, s.connID).Get()
	if err != nil {
		return nil, auth.ErrDenied // never disclose which connections exist
	}
	stmt, cerr := Classify(sqlText, connRow.Engine == "mysql")
	if cerr == nil && stmt.Class == ClassControl {
		if err := e.profileFor(connRow).admit(stmt); err != nil {
			return nil, e.rejectSession(ctx, s, ident, ip, sqlText, err)
		}
		// Transaction control needs a grant to match what it enables: an
		// open transaction is a held connection and a pending write, so the
		// floor is the same one a write needs.
		authorized, aerr := e.auth.Authorize(ctx, token, s.connID, auth.ActionWrite)
		if aerr != nil {
			return nil, e.rejectSession(ctx, s, ident, ip, sqlText, aerr)
		}
		s.mu.Lock()
		txOpen := s.txPhase != txNone
		aborted := s.txPhase == txAborted
		pinned := s.tx
		s.mu.Unlock()

		// SET and LOCK are admitted by the SESSION's state, not by the
		// profile alone, and once admitted they are real SQL that must reach
		// the server — unlike the transaction verbs, which never do.
		if statefulControlVerbs[stmt.Verb] {
			if aborted {
				return nil, e.rejectSession(ctx, s, authorized, ip, sqlText, ErrTxAborted)
			}
			if err := e.admitSessionState(ctx, s, authorized, stmt.Verb, sqlText, ip, txOpen); err != nil {
				return nil, err
			}
			runCtx, endRun := s.runContext(ctx)
			defer endRun()
			return e.run(runCtx, token, s.connID, sqlText, ip, nil, pinned)
		}

		tc, perr := ParseTxControl(sqlText)
		if perr != nil {
			return nil, e.rejectSession(ctx, s, authorized, ip, sqlText, perr)
		}
		return e.handleTxControl(ctx, s, authorized, connRow, tc, sqlText, ip)
	}

	// An ordinary statement. If a transaction is open it runs ON it; if the
	// transaction is aborted nothing runs at all, because the server would
	// answer every one of them identically and the caller deserves to be
	// told once what to do about it.
	s.mu.Lock()
	pinned, phase := s.tx, s.txPhase
	s.mu.Unlock()
	if phase == txAborted {
		return nil, e.rejectSession(ctx, s, ident, ip, sqlText, ErrTxAborted)
	}

	var pinnedTx dao.TxConn
	if pinned != nil {
		pinnedTx = pinned
	}
	runCtx, endRun := s.runContext(ctx)
	defer endRun()
	res, rerr := e.run(runCtx, token, s.connID, sqlText, ip, nil, pinnedTx)
	s.noteStatementOutcome(rerr)
	return res, rerr
}

// admitSessionState applies the stateful gate to SET and LOCK.
//
// The role floor is here rather than in the gate because it is a policy
// question, not a grammar one: SET LOCAL is admin-only by default per
// ADR-0074 §5, and LOCK takes the write floor already checked above. The §4
// sub-capability grant that would let an operator delegate SET LOCAL more
// finely is not built yet, so the default stands alone for now — which is
// the restrictive direction.
func (e *Engine) admitSessionState(
	ctx context.Context, s *session, ident auth.Identity,
	verb, sqlText, ip string, txOpen bool,
) error {
	switch verb {
	case "LOCK":
		if err := admitLock(txOpen); err != nil {
			return e.rejectSession(ctx, s, ident, ip, sqlText, err)
		}
		return nil
	case "SET":
		st, err := parseSet(sqlText)
		if err != nil {
			return e.rejectSession(ctx, s, ident, ip, sqlText, err)
		}
		if err := admitSet(st, txOpen); err != nil {
			return e.rejectSession(ctx, s, ident, ip, sqlText, err)
		}
		if ident.Role() != meta.RoleAdmin {
			return e.rejectSession(ctx, s, ident, ip, sqlText,
				fmt.Errorf("%w: SET LOCAL is admin-only by default", auth.ErrDenied))
		}
		return nil
	}
	return e.rejectSession(ctx, s, ident, ip, sqlText,
		fmt.Errorf("%w: %s", ErrStatementUnsupported, verb))
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
// The CAS decides the owner. Everything after it — cancelling, joining the
// in-flight statement, removing the registry entry, auditing — happens on the
// winner's goroutine and on no other, so a session closed by the idle reaper
// and by its client at the same moment is torn down once.
func (e *Engine) closeSession(ctx context.Context, s *session, ip, reason string) {
	if !s.beginClose() {
		return // someone else owns the teardown
	}
	e.finishClosing(ctx, s, ip, reason)
}

// retryClose resumes a close that could not finish because the session's
// statement would not stop. The session is already in the closing state and
// still owns its transaction; this is the retry that eventually ends it.
func (e *Engine) retryClose(ctx context.Context, s *session) {
	ip, reason := s.closeReason()
	e.finishClosing(ctx, s, ip, reason)
}

func (e *Engine) finishClosing(ctx context.Context, s *session, ip, reason string) {
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
	tx, txID := s.tx, s.txID
	if quiesced == nil {
		// Detach only when it is safe to. Leaving the transaction attached
		// to a session that is going away is the lesser evil: the pool's
		// teardown and the server-side belt both end it, whereas rolling
		// back underneath a live statement corrupts the connection state for
		// whatever the pool hands out next.
		s.tx, s.txPhase, s.txID = nil, txNone, ""
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
		s.setCloseReason(ip, reason)
		return
	}
	if tx != nil {
		// A FRESH bounded context: the session's own is cancelled by now, and
		// a rollback that cannot run because its context is gone would leave
		// a transaction holding locks on a live database with nobody left to
		// close it.
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
		rerr := tx.RollbackContext(cctx)
		ccancel()
		outcome := "rolled_back"
		if rerr != nil {
			outcome = "rollback_failed"
			e.logf("session %s: rolling back %s on close: %v", s.id, txID, rerr)
		}
		e.auditBounded(ctx, s.userID, ip, "tx_"+outcome,
			fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, reason))
	}

	// The session's own context is cancelled last, once nothing is running on
	// it and the rollback has had its fresh context.
	s.cancel()

	e.sessions.remove(s)
	s.finishClose()

	e.auditBounded(ctx, s.userID, ip, "session_closed",
		fmt.Sprintf("conn %d: session %s: %s", s.connID, s.id, reason))
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

// reapIdleSessions closes sessions idle beyond the timeout. It is called by
// the engine's janitor and is exported to the package's tests through a clock
// rather than a sleep.
func (e *Engine) reapIdleSessions(ctx context.Context, now time.Time) int {
	var n int
	for _, s := range e.sessions.snapshot() {
		if s.idleFor(now) >= e.sessionIdle {
			e.closeSession(ctx, s, "", "idle-timeout")
			n++
		}
	}
	return n
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
