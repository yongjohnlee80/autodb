package exec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
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
		s.mu.Lock()
		phase, opened, last, txID := s.txPhase, s.txOpened, s.lastUsed, s.txID
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
			if e.revokeExpiredAuthority(ctx, s, txID) {
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
	if err := e.quiesce(ctx, s, quiesceTimeout); err != nil {
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
		return
	}

	s.mu.Lock()
	tx := s.tx
	s.tx, s.txPhase, s.txID = nil, txNone, ""
	s.lastUsed = e.now()
	s.mu.Unlock()
	if tx == nil {
		return
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCleanupTimeout)
	err := tx.RollbackContext(cctx)
	cancel()

	outcome := "rolled_back"
	if err != nil {
		outcome = "rollback_failed"
		e.logf("session %s: rolling back %s on %s: %v", s.id, txID, reason, err)
	}
	e.auditBounded(ctx, s.userID, "", "tx_"+outcome,
		fmt.Sprintf("conn %d: session %s: %s: %s", s.connID, s.id, txID, reason))
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
func (e *Engine) revokeExpiredAuthority(ctx context.Context, s *session, txID string) bool {
	err := e.auth.StillAuthorized(ctx, s.authSessID, s.userID, s.connID, auth.ActionWrite)
	if err == nil {
		return false
	}
	if !errors.Is(err, auth.ErrDenied) {
		e.logf("session %s: re-checking authority failed, leaving the transaction to its timeouts: %v", s.id, err)
		return false
	}
	e.rollbackExpired(ctx, s, txID, reasonAuthorityRevoked)
	// And the session goes with it. Leaving it open would leave a caller
	// holding a handle to a connection they are no longer entitled to, ready
	// to BEGIN again the moment a grant reappeared.
	e.closeSession(ctx, s, "", reasonAuthorityRevoked)
	return true
}
