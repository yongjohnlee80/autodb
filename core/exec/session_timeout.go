package exec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Transaction timeouts (ADR-0074 §1, Amendment 2 C2).
//
// These exist because the target may be a live production database. A
// transaction abandoned between BEGIN and COMMIT — a client that crashed, a
// developer who went to lunch, a panel left open — holds its locks until
// something ends it, and nothing else will. That makes an abandoned
// transaction an incident rather than an inconvenience, which is why the
// bounds have real defaults instead of being a knob somebody might not set.
//
// TWO BELTS, IN ORDER. The engine's own deadline fires FIRST, on the path
// that can audit what happened. The server-side
// idle_in_transaction_session_timeout is set slightly LATER, so it only ever
// wins when autodb itself is gone — in which case there is no audited
// rollback to write, and the outcome is reconciled rather than fabricated.
// Setting them the other way round would let the server win the race and
// leave the engine reporting a rollback it did not perform.

// txLimits are the bounds on one session's transaction.
type txLimits struct {
	idleInTx time.Duration
	maxTx    time.Duration
	// serverBeltMargin is how far behind the engine deadline the server-side
	// belt is set.
	serverBeltMargin time.Duration
}

// defaultTxLimits mirrors the config defaults.
func defaultTxLimits() txLimits {
	return txLimits{
		idleInTx:         90 * time.Second,
		maxTx:            5 * time.Minute,
		serverBeltMargin: 30 * time.Second,
	}
}

// forConnection applies a per-connection override, capped by the install-wide
// ceiling. A connection row must not be able to raise its own limit past what
// the operator decided, or the bound is advisory.
func (l txLimits) forConnection(debug bool, debugIdle, ceiling time.Duration) txLimits {
	out := l
	if debug && debugIdle > 0 {
		out.idleInTx = debugIdle
	}
	if ceiling > 0 && out.maxTx > ceiling {
		out.maxTx = ceiling
	}
	return out
}

// expiredReason reports which limit a transaction has passed, or "".
//
// Idle is checked before duration so the message names the limit a user can
// most readily act on: "you left it open" is more useful than "it ran long"
// when both are true.
func (l txLimits) expiredReason(now, lastActivity, opened time.Time) string {
	if l.idleInTx > 0 && now.Sub(lastActivity) >= l.idleInTx {
		return "idle-in-transaction"
	}
	if l.maxTx > 0 && now.Sub(opened) >= l.maxTx {
		return "max-transaction-duration"
	}
	return ""
}

// serverBeltSeconds is what the server-side belt is set to: the engine's own
// idle deadline plus a margin, so the engine always fires first.
func (l txLimits) serverBeltSeconds() int {
	return int((l.idleInTx + l.serverBeltMargin).Seconds())
}

// armServerBelt sets the target's own idle-in-transaction guard on the pinned
// transaction, so the rollback still happens if autodb dies.
//
// SET LOCAL, deliberately: it reverts at the transaction boundary by
// PostgreSQL semantics, so nothing can outlive the pin and leak onto a
// pooled connection. It is engine-originated, the same category as the
// grammar verification, and is not the user's SET — which stays refused.
//
// A target that does not support it is not an error. The belt is a
// belt: the engine's own deadline is the guarantee, and a driver without the
// GUC simply does not get the second layer.
func armServerBelt(ctx context.Context, tx dao.TxConn, engineName string, l txLimits) error {
	if engineName != "postgres" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = '%ds'", l.serverBeltSeconds()))
	return err
}

// connectionIsDebug reports whether a connection carries the debug profile,
// which gets the longer idle bound (ADR-0074 Amendment 2 C2).
func connectionIsDebug(row *meta.Connection) bool { return row != nil && row.IsDebug() }

// reapExpired rolls back transactions past their limits and closes idle
// sessions. It takes the clock so tests drive it directly instead of sleeping
// through a 90-second timeout.
//
// It returns how many sessions it acted on.
func (e *Engine) reapExpired(ctx context.Context, now time.Time) int {
	var acted int
	for _, s := range e.sessions.snapshot() {
		// A session stuck in the closing state still owns a transaction: its
		// close skipped the rollback because the statement would not stop,
		// and retained the session precisely so this retry exists. Without
		// it the skip would be permanent and the transaction would hold
		// locks with no owner able to end it.
		if s.get() == sessClosing {
			if e.retryClose(ctx, s) {
				acted++
			}
			continue
		}
		s.mu.Lock()
		tx, phase, opened, last, txID := s.tx, s.txPhase, s.txOpened, s.lastUsed, s.txID
		openedMayWrite := s.txOpenedMayWrite
		limits := s.limits
		s.mu.Unlock()

		if phase != txNone {
			// Authority is re-checked BEFORE the clock. A revoked authority
			// is not a slower version of a timeout: the transaction is
			// holding locks on a target the caller is no longer entitled to
			// touch, and waiting out the remaining minutes of its budget
			// keeps it holding them.
			//
			// Denying the caller's NEXT statement is not enough, and was
			// what happened before. A client that simply stops talking after
			// BEGIN never sends a next statement, so a revoked user's
			// transaction stayed open for its full duration — and re-adding
			// the grant let them carry on as if nothing had been revoked.
			if e.revokeExpiredAuthority(ctx, s, tx, phase, txID, openedMayWrite) {
				acted++
				continue
			}
			reason := limits.expiredReason(now, last, opened)
			if reason == "" {
				continue
			}
			e.rollbackExpired(ctx, s, txID, reason)
			acted++
			continue
		}
		// No transaction: the session's own idle timeout applies. This is
		// what reaps a session whose client crashed without closing it.
		if s.idleFor(now) >= e.sessionIdle {
			e.closeSession(ctx, s, "", "idle-timeout")
			acted++
		}
	}
	return acted
}

// rollbackExpired ends one transaction that has passed a limit, audited with
// WHICH limit fired — the ADR is explicit that a timeout rollback is
// distinguishable from every other kind.
// reasonAuthorityRevoked is the audited reason for a rollback forced by an
// authority ending rather than by a clock. It is a distinct reason on purpose:
// an operator reading the audit needs to see that a transaction was ended
// because permission was withdrawn, not because someone was slow.
const reasonAuthorityRevoked = "authority-revoked"

func (e *Engine) rollbackExpired(ctx context.Context, s *session, txID, reason string) {
	// Cancel, then JOIN, then roll back. Without this the rollback was
	// issued while a statement was still executing on the same connection —
	// two commands in flight at once. The timeout path never did either: it
	// took the transaction out from under whatever was running and rolled it
	// back concurrently.
	release, err := e.quiesce(ctx, s, e.txQuiesce)
	defer release()
	if err != nil {
		// The statement did not stop. Rolling back now would be the exact
		// concurrent-command bug, so the transaction is LEFT ATTACHED and
		// the next sweep tries again — with the server-side belt as the
		// backstop underneath it. Left visible rather than silent, because a
		// statement that ignores cancellation is itself worth knowing about.
		e.logf("session %s: %s expired but the in-flight statement would not stop (%v); "+
			"leaving the transaction for the next sweep", s.id, txID, err)
		e.auditBounded(ctx, s.userID, "", "tx_rollback_deferred",
			fmt.Sprintf("conn %d: session %s: %s: %s: in-flight statement did not stop",
				s.connID, s.id, txID, reason))
		// The transaction is still live and still owned, so its outcome is
		// genuinely undetermined right now — which is a thing the log must
		// SAY rather than imply by omission. Without this the entry would sit
		// at `opened` while holding locks, and `opened` reads as a healthy
		// transaction in progress. The next sweep resolves it; the append is
		// idempotent, so repeated sweeps do not grow the log.
		e.noteTxOutcome(ctx, txTransition{
			txID: txID, state: meta.TxUnknownPending, reason: meta.ReasonTimeout,
			userID: s.userID, connectionID: s.connID,
		})
		return
	}

	s.mu.Lock()
	tx := s.tx
	s.clearTxLocked()
	s.lastUsed = e.now()
	s.mu.Unlock()
	if tx == nil {
		return
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	rerr := tx.RollbackContext(cctx)
	cancel()

	outcome := "rolled_back"
	if rerr != nil {
		outcome = "rollback_failed"
		e.logf("session %s: rolling back %s on %s: %v", s.id, txID, reason, rerr)
	}
	// The terminal that resolves any unknown_pending a previous deferred
	// sweep left behind.
	e.noteTxOutcome(ctx, txTransition{
		txID: txID, state: txStateFor(outcome, rerr), reason: txOutcomeReason(outcome, rerr),
		userID: s.userID, connectionID: s.connID,
	})
	e.auditBounded(ctx, s.userID, "", "tx_"+outcome,
		fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, reason))
}

// reasonAuthorityDemoted is the audited reason for a transaction ended because
// WRITE privilege was withdrawn while the session's own right to be connected
// survived.
//
// Distinct from authority-revoked on purpose, and lector required the
// distinction: an operator reading the trail needs to see that a still-valid
// reader lost write privilege, not that someone's access ended. The two lead
// to different conversations.
const reasonAuthorityDemoted = "authority-demoted"

// reasonDemotionCleanupFailed is a close reason, not a fabricated revocation.
const reasonDemotionCleanupFailed = "demotion-cleanup-failed"

// enforceTransactionAuthority runs in a foreground caller that already owns
// the session slot. It synchronously ends a transaction opened with write
// authority when this unit's fresh policy is read-only.
func (e *Engine) enforceTransactionAuthority(
	ctx context.Context, s *session, pol UnitPolicy, ip string,
) (bool, error) {
	if pol.MayWrite {
		return false, nil
	}
	s.mu.Lock()
	tx, phase, txID := s.tx, s.txPhase, s.txID
	s.mu.Unlock()
	return e.rollbackDemotedOwned(ctx, s, tx, phase, txID, pol.Role, ip)
}

// rollbackDemoted first acquires teardown ownership for the janitor, then uses
// the same primitive as foreground preflight. expected* is the janitor's
// snapshot; a foreground winner makes the primitive a silent no-op.
func (e *Engine) rollbackDemoted(
	ctx context.Context, s *session, expectedTx dao.ContextTxConn,
	expectedPhase txPhase, expectedTxID, role string,
) bool {
	if h := e.hookBeforeDemotionQuiesce; h != nil {
		h()
	}
	release, err := e.quiesce(ctx, s, e.txQuiesce)
	if err != nil {
		// Two materially different failures reach here, and only one may
		// close the session. A join that did not complete means the
		// statement would not stop and this sweep cannot ever clean up:
		// close, so the normal close path owns the terminal outcome.
		if errors.Is(err, ErrSessionBusy) {
			// Claim contention, not cleanup failure. Join succeeded, and a
			// foreground caller claimed the slot before this sweep could.
			// That caller is the correct linearization owner: it runs the
			// same preflight and either rolls the transaction back or
			// observes no-match. Closing here would end a healthy retained
			// reader session for losing a race the lifecycle already
			// resolves in the foreground's favor — so defer instead, and a
			// later sweep re-checks if the foreground itself fails.
			e.logf("session %s: write privilege was withdrawn but a foreground caller "+
				"claimed the slot first; deferring demotion rollback to it", s.id)
			return false
		}
		e.logf("session %s: write privilege was withdrawn but the in-flight statement would not "+
			"stop (%v); closing for demotion cleanup failure", s.id, err)
		e.closeSession(ctx, s, "", reasonDemotionCleanupFailed)
		return true
	}

	acted, rerr := e.rollbackDemotedOwned(ctx, s, expectedTx, expectedPhase, expectedTxID, role, "")
	closeOwner := false
	if rerr != nil {
		// Own the terminal transition while teardown still owns the slot, so
		// no foreground unit can enter between failure and close.
		closeOwner = e.transferDemotionClose(s, "")
	}
	release()
	if closeOwner {
		e.finishClosing(context.WithoutCancel(ctx), s)
	}
	return acted
}

// rollbackDemotedOwned requires the caller to own the foreground/teardown
// slot and to have no target statement installed. It never quiesces or claims
// another slot, so foreground preflight cannot cancel or join itself.
func (e *Engine) rollbackDemotedOwned(
	ctx context.Context, s *session, expectedTx dao.ContextTxConn,
	expectedPhase txPhase, expectedTxID, role, ip string,
) (bool, error) {
	s.mu.Lock()
	if !s.busy || s.runCancel != nil {
		s.mu.Unlock()
		return false, errors.New("exec: demotion rollback called without sole idle-slot ownership")
	}
	if expectedTx == nil || expectedPhase == txNone || s.tx != expectedTx ||
		s.txPhase != expectedPhase || s.txID != expectedTxID || !s.txOpenedMayWrite {
		s.mu.Unlock()
		return false, nil
	}
	teardown := s.tearingDown
	s.mu.Unlock()
	if h := e.hookDemotionOwned; h != nil {
		h(teardown)
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	rerr := expectedTx.RollbackContext(cctx)
	cancel()
	if rerr != nil {
		e.logf("session %s: rolling back %s after demotion: %v", s.id, expectedTxID, rerr)
		// Leave every transaction field attached. The caller transfers the
		// still-owned transaction to finishClosing, which alone records the
		// terminal cleanup outcome.
		return true, rerr
	}

	s.mu.Lock()
	if s.tx != expectedTx || s.txPhase != expectedPhase || s.txID != expectedTxID ||
		!s.txOpenedMayWrite {
		s.mu.Unlock()
		return true, errors.New("exec: transaction identity changed while demotion rollback owned the session slot")
	}
	s.clearTxLocked()
	s.lastUsed = e.now()
	s.demoted = true
	s.mu.Unlock()

	e.noteTxOutcome(ctx, txTransition{
		txID: expectedTxID, state: meta.TxRolledBack,
		userID: s.userID, connectionID: s.connID,
	})
	e.auditBounded(ctx, s.userID, ip, "tx_rolled_back",
		fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, expectedTxID, reasonAuthorityDemoted))
	e.auditBounded(ctx, s.userID, ip, reasonAuthorityDemoted,
		fmt.Sprintf("conn %d: session %s: write privilege withdrawn (role now %s); the session "+
			"continues at the read floor", s.connID, s.id, role))
	return true, nil
}

// quiesceTimeout bounds how long a teardown waits for an in-flight statement
// to stop after being cancelled. With the statement running on a context the
// engine controls, cancellation reaches the driver, so this is an outer bound
// on a fast path rather than an expected wait.
const quiesceTimeout = 10 * time.Second

// StartJanitor runs the reaper until ctx is done. The daemon calls it once;
// tests call reapExpired directly with their own clock.
func (e *Engine) StartJanitor(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 15 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.reapExpired(ctx, e.now())
			}
		}
	}()
}

// revokeExpiredAuthority rolls back and closes a session whose authority has
// ended, and reports whether it acted.
//
// It covers every way an authority can end — logout, an admin revoking the
// user's sessions, expiry, the user being disabled, the grant on this
// connection being removed or lowered — because a rollback that fires for a
// removed grant but not for a disabled user is a coincidence rather than a
// guarantee. The write floor is used deliberately: an open transaction is a
// held connection and a pending write, so the authority it needs to CONTINUE
// is the one it needed to begin.
//
// A store failure is not a revocation. If the meta store cannot be read, the
// authority is left standing and the timeouts still bound the transaction —
// tearing down live work because a lookup failed would turn a blip in the
// meta store into rolled-back transactions across every open session.
func (e *Engine) revokeExpiredAuthority(
	ctx context.Context, s *session, tx dao.ContextTxConn, phase txPhase,
	txID string, openedMayWrite bool,
) bool {
	v, err := e.auth.ResolveStanding(ctx, s.authority, s.userID, s.connID)
	if err != nil {
		// A store failure is NOT a revocation. Tearing down live work
		// because a lookup failed would turn a blip in the meta store into
		// rolled-back transactions across every open session; the
		// transaction timeouts remain the backstop.
		e.logf("session %s: re-checking authority failed, leaving the transaction to its timeouts: %v", s.id, err)
		return false
	}
	if !v.Standing {
		e.rollbackExpired(ctx, s, txID, reasonAuthorityRevoked)
		// And the session goes with it. Leaving it open would leave a caller
		// holding a handle to a connection they are no longer entitled to,
		// ready to BEGIN again the moment a grant reappeared.
		e.closeSession(ctx, s, "", reasonAuthorityRevoked)
		return true
	}
	if v.MayWrite {
		return false
	}
	if !openedMayWrite {
		// A transaction opened under reader policy is already server-enforced
		// read-only. A later reader verdict changes nothing and must not replace
		// the transaction with an autocommit unit.
		return false
	}

	// DEMOTION, not revocation, and the difference is what happens to the
	// session (lector's ruling on the standing-authority defect).
	//
	// The credential is valid and the read grant stands; only the write
	// privilege is gone. Killing the connection for a privilege REDUCTION
	// would be harsher than what happens when a grant is removed from a
	// session with no open transaction — so the TRANSACTION ends and the
	// session survives it.
	//
	// The open transaction cannot continue either way: it was begun with
	// write authority and the next operation would be running as a reader on
	// a read-write transaction, which is the state the seam condition
	// forbids.
	return e.rollbackDemoted(ctx, s, tx, phase, txID, v.Role)
}
