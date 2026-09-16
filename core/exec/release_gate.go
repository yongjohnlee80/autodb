package exec

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// A BACKEND MAY GO BACK TO THE POOL ONLY WHEN A RESET HAS PROVED IT CARRIES
// NOTHING FORWARD.
//
// The next borrower of a pooled backend is, in this product, a different
// developer on a different day. Everything the previous borrower left behind on
// that backend — a changed setting, a temporary table, a prepared statement, a
// held cursor, an advisory lock, a LISTEN registration, an unfinished
// transaction — becomes state that person did not create and cannot see. The
// failures that follow are the worst kind to diagnose: a query that reads the
// wrong search_path, a NOTIFY delivered to a session that never listened, an
// advisory lock nobody can find the holder of.
//
// So the rule is not "clean it up on the way out and hope". It is that the
// backend is handed back to the pool ONLY when every reset statement completed,
// the target answered none of them with an error, and the connection reported
// itself idle with no transaction open afterwards. Any other ending — a reset
// that failed, a wire that broke, a transaction still attached, a status byte
// that is not idle, a pool handback the driver refused — closes the physical
// connection instead.
//
// DISCARDING IS THE CORRECT ANSWER, NOT THE FAILURE ANSWER. A discarded backend
// costs one reconnect, paid once, by nobody in particular. A wrongly pooled one
// hands a stranger's session state to the next person and nothing in the system
// will ever say so. Uncertainty therefore resolves to discard, including
// uncertainty this file cannot name yet.
//
// WHAT THIS GATE DOES NOT COVER, said plainly so nobody reads more into it: it
// governs the backends autodb itself pins for a front-door session and later
// hands back. Statements executed through the ordinary pooled path (conns.go)
// are given a connection by the driver's own pool and returned to it by the
// driver, with no seam here; a routine called from such a statement can still
// change session state on a connection this gate never sees. Closing that is a
// separate piece of work on the pool's acquire hook, not something to bolt onto
// the session release path.

// resetStep is ONE kind of state a backend can carry forward, paired with the
// statement that removes it.
//
// PostgreSQL offers DISCARD ALL, which does all of this in one statement, and
// the reset is written out step by step anyway. The reason is that a single
// statement cannot be tested: there is no way to run DISCARD ALL without its
// temp-object clause, so there is no way to show that the temp-object clause is
// what stops a temporary table reaching the next borrower. A reset nobody can
// break on purpose is a reset nobody has proved. With the steps enumerated, a
// test removes exactly one and watches exactly one leak appear.
type resetStep struct {
	// carries names the state in the words an operator would use for it, and
	// is what an audit record says when the step is the one that failed.
	carries string
	// sql removes that state from the backend.
	sql string
}

// backendResetPlan is the whole reset, in the order PostgreSQL uses for DISCARD
// ALL itself. The order is copied rather than invented: cursors are closed
// before settings are reset, and the temp schema is dropped last, because that
// is the sequence the server's own implementation is written and tested in.
//
// Every step is a statement the simple-query face may carry. None of them is
// transaction control, which matters because that face refuses transaction
// control outright and poisons the connection for trying — the open
// transaction is ended by the session's own owner before this plan ever runs.
func backendResetPlan() []resetStep {
	return []resetStep{
		{"open cursors and held portals", "CLOSE ALL"},
		{"a changed session authorization", "SET SESSION AUTHORIZATION DEFAULT"},
		{"session settings the borrower changed", "RESET ALL"},
		{"prepared statements, named and unnamed", "DEALLOCATE ALL"},
		{"LISTEN registrations", "UNLISTEN *"},
		{"advisory locks held for the life of the backend", "SELECT pg_advisory_unlock_all()"},
		{"cached plans built under the borrower's settings", "DISCARD PLANS"},
		{"sequence state cached for currval", "DISCARD SEQUENCES"},
		{"temporary tables and everything else in the temp schema", "DISCARD TEMP"},
	}
}

// resetPlan returns the plan this engine will run. The field is empty in
// production and a test fills it to run a DELIBERATELY INCOMPLETE reset, which
// is the only way to show that each step is load-bearing rather than decorative.
func (e *Engine) resetPlan() []resetStep {
	if len(e.backendReset) > 0 {
		return e.backendReset
	}
	return backendResetPlan()
}

// backendResetTimeout bounds the whole reset. It is generous because the reset
// runs while a session is closing and a slow answer is not yet a wrong one, and
// it is bounded because a reset that never returns must not keep a closing
// session alive: the timeout simply becomes a discard, which is safe.
const backendResetTimeout = 10 * time.Second

// The limbs of the gate, named so a discard can be read back afterwards
// instead of guessed at.
const (
	releaseLimbTransaction = "transaction"
	releaseLimbDrain       = "drain"
	releaseLimbFace        = "simple-query face"
	releaseLimbReset       = "reset"
	releaseLimbIdle        = "idle status"
	releaseLimbHandback    = "pool handback"
)

// releaseVerdict is what the gate decided about one backend, and why.
type releaseVerdict struct {
	// pooled is true only when the backend was proved clean AND the driver
	// accepted it back. Everything else leaves it false.
	pooled bool
	// limb is the part of the gate that refused, empty when pooled.
	limb string
	// detail is the operator-facing reason, empty when pooled.
	detail string
}

// reason renders the verdict for an audit record.
func (v releaseVerdict) reason() string {
	if v.pooled {
		return "reset verified; returned to the pool"
	}
	return v.limb + ": " + v.detail
}

// releaseBackend is THE ONE PLACE a pinned backend's fate is decided, and the
// only place in this package that hands a pinned connection back to the pool.
//
// txResidue is what the caller already knows and the wire cannot show: the
// error from ending the session's transaction, or nil when there was nothing to
// end or it ended cleanly. It is a parameter rather than something re-derived
// here because the owner that ran the rollback is the only code that saw its
// result, and a gate that guessed would pool a backend whose rollback failed.
//
// The backend is always relinquished: either the pool takes it back or the
// physical connection is closed. There is no path out of this function that
// leaves the lease held.
func (e *Engine) releaseBackend(ctx context.Context, s *session, pc golibpg.PinnedConn, txResidue error) releaseVerdict {
	v := e.proveBackendClean(ctx, s, pc, txResidue)
	if !v.pooled {
		discardBackend(ctx, pc)
		return v
	}
	if err := pc.Release(ctx); err != nil {
		// The driver refused the handback — it disagrees with us about the
		// connection's state. Its opinion wins: it is the side holding the
		// socket. Discarding is idempotent, so this is safe even if the
		// refusal happened after the lease had already gone.
		discardBackend(ctx, pc)
		return releaseVerdict{limb: releaseLimbHandback, detail: err.Error()}
	}
	return v
}

// discardBackend destroys the physical connection instead of letting the pool
// keep it.
//
// GIVING THE LEASE BACK IS NOT THE SAME AS CLOSING THE SOCKET, and this cost a
// live cell to discover. The driver's discard relinquishes the lease and then
// decides for itself whether the member can be reused, and that decision is
// made on wire mechanics alone: nothing in flight, nothing poisoned, no
// transaction the driver itself opened. A backend whose reset the server just
// refused passes every one of those tests — the wire is in perfect order and
// the session state is exactly what we failed to clear — so the connection goes
// straight back into the pool, which is the outcome this whole file exists to
// prevent.
//
// The driver cannot be blamed for that: session state is not something it can
// see. So the connection is put into a state the driver CAN see is unprovable.
// Queueing a frame and never flushing it leaves the outbound track mid-frame,
// which is exactly the condition its reuse test treats as unreusable, and it
// costs no round trip — the frame is buffered locally and the socket is closed
// before anything is written. The frame chosen is a close of the unnamed portal
// precisely because it would be harmless if it ever did reach a server.
//
// Every way the queueing can fail already means the handle is terminal or
// mid-exchange, which is itself unreusable, so the error is not worth
// inspecting: either way the connection cannot be recycled.
func discardBackend(ctx context.Context, pc golibpg.PinnedConn) {
	_ = pc.Send(ctx, golibpg.ClosePortalOp(""))
	pc.Discard()
}

// proveBackendClean runs the gate's limbs in order and reports the first one
// that could not be proved. A limb that cannot prove its condition does not try
// the next one: a reset issued on a connection with an open transaction would
// fail anyway, and issuing it would only replace a clear reason with a confusing
// one.
func (e *Engine) proveBackendClean(ctx context.Context, s *session, pc golibpg.PinnedConn, txResidue error) releaseVerdict {
	if txResidue != nil {
		return releaseVerdict{limb: releaseLimbTransaction, detail: txResidue.Error()}
	}
	if s != nil {
		s.mu.Lock()
		stillOwned := s.tx != nil
		s.mu.Unlock()
		if stillOwned {
			// The session is being torn down with its transaction still
			// attached, which means the owner could not end it. Whatever that
			// transaction holds — locks, an unflushed write, an aborted block
			// — would be the next borrower's inheritance.
			return releaseVerdict{limb: releaseLimbTransaction,
				detail: "the session's transaction is still attached"}
		}
	}
	if v := e.drainWire(ctx, s, pc); !v.pooled {
		return v
	}
	return e.runBackendReset(ctx, pc)
}

// runBackendReset runs the plan and reports the first step that could not be
// completed. It is shared by the two moments a backend's cleanliness has to be
// established — when a session gives one up, and when a session takes one — so
// that "clean" cannot come to mean two different things.
func (e *Engine) runBackendReset(ctx context.Context, pc golibpg.PinnedConn) releaseVerdict {
	sq, ferr := rawFace(pc)
	if ferr != nil {
		// Without a simple-query face there is no way to run a reset at all,
		// so there is no way to prove anything. That is a discard, not an
		// exemption.
		return releaseVerdict{limb: releaseLimbFace, detail: ferr.Error()}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backendResetTimeout)
	defer cancel()
	for _, step := range e.resetPlan() {
		var targetErr *pgconn.PgError
		status, derr := e.sessionSimpleQuery(rctx, sq, sessionSQLAutodb, step.sql,
			func(m golibpg.ExtendedMessage) error {
				if m.Kind == "ErrorResponse" && targetErr == nil {
					targetErr = m.Err
				}
				return nil
			})
		switch {
		case derr != nil:
			return releaseVerdict{limb: releaseLimbReset,
				detail: fmt.Sprintf("%s: %v", step.carries, derr)}
		case targetErr != nil:
			// The target refused the statement. The state it names is still
			// on that backend, so nothing was proved.
			return releaseVerdict{limb: releaseLimbReset,
				detail: fmt.Sprintf("%s: %s", step.carries, targetErr.Message)}
		case status != TxStatusIdle:
			// The reset ran and the connection still says it is inside a
			// transaction. A reset statement cannot open one, so something
			// else did, and a connection whose state we cannot explain is
			// exactly what must not be pooled.
			return releaseVerdict{limb: releaseLimbIdle,
				detail: fmt.Sprintf("%s: the connection reported %q after %s",
					step.carries, string(rune(status)), step.sql)}
		}
	}
	return releaseVerdict{pooled: true}
}

// drainWire ends an extended-protocol exchange the client left open.
//
// A client that queues frames and then disappears without a Sync leaves the
// connection mid-exchange, with answers the server has not finished sending.
// Nothing can run on it in that state — the simple-query face refuses, and so
// does the pool handback — so the exchange is ended here with the one call that
// returns the wire to rest, and the private read-only wrap an exchange may be
// running inside is rolled back after it, exactly as an ordinary end of exchange
// does it.
func (e *Engine) drainWire(ctx context.Context, s *session, pc golibpg.PinnedConn) releaseVerdict {
	if s == nil {
		return releaseVerdict{pooled: true}
	}
	s.mu.Lock()
	ext := s.ext
	open := ext != nil && len(ext.segment) > 0
	s.mu.Unlock()
	if ext == nil {
		return releaseVerdict{pooled: true}
	}
	var status byte = TxStatusIdle
	var serr error
	if open {
		status, serr = pc.Sync(ctx)
	}
	s.mu.Lock()
	ext.segment = nil
	ext.sweepUnfinalized()
	ext.releaseReadOnlyWrap(ctx)
	s.mu.Unlock()
	switch {
	case serr != nil:
		return releaseVerdict{limb: releaseLimbDrain, detail: serr.Error()}
	case status != TxStatusIdle:
		// The client's unfinished exchange left a transaction open. The
		// session owner's rollback has already run by this point, so there is
		// nobody left to end it and the backend cannot be proved clean.
		return releaseVerdict{limb: releaseLimbDrain,
			detail: fmt.Sprintf("the unfinished exchange ended with the connection reporting %q",
				string(rune(status)))}
	}
	return releaseVerdict{pooled: true}
}

// noteBackendFate records what happened to the backend, with the reason in the
// record rather than only in a log line. An operator investigating "why does
// this target keep reconnecting" needs to be able to read the limb that refused.
func (e *Engine) noteBackendFate(ctx context.Context, s *session, ip string, v releaseVerdict) {
	action := "backend_discarded"
	if v.pooled {
		action = "backend_pooled"
	}
	e.auditBounded(ctx, s.userID, ip, action,
		fmt.Sprintf("conn %d: session %s: %s", s.connID, s.id, v.reason()))
	if !v.pooled {
		e.logf("session %s: backend not returned to the pool (%s)", s.id, v.reason())
	}
}

// resetPlanWithout returns the plan with one step removed, for the cells that
// prove each step is what stops its own leak. It lives beside the plan rather
// than in a test file so the plan and the mutation of it cannot drift apart:
// a step renamed here and not there would silently remove nothing, and a
// mutation that removes nothing passes.
func resetPlanWithout(carries string) []resetStep {
	full := backendResetPlan()
	out := make([]resetStep, 0, len(full))
	for _, step := range full {
		if strings.EqualFold(step.carries, carries) {
			continue
		}
		out = append(out, step)
	}
	return out
}

// proveCheckoutClean is the OTHER end of the contract: a session must not begin
// work on a backend it cannot prove is clean.
//
// The release gate alone is not enough, and the reason is the pool this front
// door shares with everything else autodb runs. An ordinary statement executed
// outside any session borrows from the SAME pool and is returned to it by the
// driver, with no seam here and no reset — and a statement can change session
// state without looking like it does: a plain SELECT that calls a routine which
// sets a configuration parameter leaves that parameter on the backend, and the
// next borrower inherits it. A live cell in this package demonstrates exactly
// that inheritance.
//
// So a session proves the backend it was handed, rather than trusting where it
// came from. Failing the proof fails the pin: the client is told the target
// could not give it a usable session, which is true, and is a far better answer
// than a session quietly running under somebody else's settings.
func (e *Engine) proveCheckoutClean(ctx context.Context, pc golibpg.PinnedConn) error {
	v := e.runBackendReset(ctx, pc)
	if v.pooled {
		return nil
	}
	discardBackend(ctx, pc)
	return fmt.Errorf("exec: the backend taken for this session could not be proved clean (%s)", v.reason())
}
