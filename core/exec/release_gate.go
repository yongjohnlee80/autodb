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
// THE RULE HOLDS ON BOTH KINDS OF HANDBACK, and it has to. A front-door session
// hands its pinned backend back through this file. An ordinary statement —
// anything run outside a session — is given a connection by the driver's own
// pool and returned to it by the driver, with no seam here at all; and a
// statement can change session state without looking like it does, because a
// plain SELECT that calls a routine which sets a configuration parameter leaves
// that parameter behind. Sanitizing only the session path would leave
// ordinary→ordinary as a cross-developer leak in the same shared pool, so the
// plan below also runs on the driver's release hook (dsn.go). Both places drive
// THE SAME plan through THE SAME verdict logic, deliberately: a second
// implementation would be a second definition of "clean", and the two would
// drift until one of them was wrong.

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
	// invalidatesDriverCache marks a step whose effect the DRIVER also mirrors
	// on its own side, so a transport that keeps such a mirror must carry this
	// step through the driver's own call rather than straight down the wire.
	//
	// Exactly one step is like this, and it is the one that deallocates
	// prepared statements: pgx caches a prepared statement per connection and
	// names it from a hash of the SQL text, so a bare DEALLOCATE ALL removes the
	// server's copy while leaving the driver certain its own still exists. The
	// next statement with that text then fails on a connection the pool
	// considers healthy, for a caller who did nothing wrong.
	//
	// The flag says WHAT is true of the step, not what any transport should do
	// about it. The pinned face has no such cache and ignores it; the pooled
	// runner (dsn.go) is the one that acts on it.
	invalidatesDriverCache bool
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
		{carries: "open cursors and held portals", sql: "CLOSE ALL"},
		{carries: "a changed session authorization", sql: "SET SESSION AUTHORIZATION DEFAULT"},
		{carries: "session settings the borrower changed", sql: "RESET ALL"},
		{carries: "prepared statements, named and unnamed", sql: "DEALLOCATE ALL",
			invalidatesDriverCache: true},
		{carries: "LISTEN registrations", sql: "UNLISTEN *"},
		{carries: "advisory locks held for the life of the backend",
			sql: "SELECT pg_advisory_unlock_all()"},
		{carries: "cached plans built under the borrower's settings", sql: "DISCARD PLANS"},
		{carries: "sequence state cached for currval", sql: "DISCARD SEQUENCES"},
		{carries: "temporary tables and everything else in the temp schema", sql: "DISCARD TEMP"},
	}
}

// resetPlan returns the plan this engine will run. The field is empty in
// production and a test fills it to run a DELIBERATELY INCOMPLETE reset, which
// is the only way to show that each step is load-bearing rather than decorative.
func (e *Engine) resetPlan() []resetStep {
	if p := e.backendReset.Load(); p != nil && len(*p) > 0 {
		return *p
	}
	return backendResetPlan()
}

// setResetPlan installs a deliberately incomplete plan. Test-only; a nil plan
// restores the production one. It exists so the plan is published atomically,
// because the pool's release hook reads it from pgxpool's own goroutine.
func (e *Engine) setResetPlan(plan []resetStep) {
	if plan == nil {
		e.backendReset.Store(nil)
		return
	}
	e.backendReset.Store(&plan)
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
		e.destroyBackend(ctx, pc)
		return v
	}
	if err := pc.Release(ctx); err != nil {
		// The driver refused the handback — it disagrees with us about the
		// connection's state. Its opinion wins: it is the side holding the
		// socket. Destroying is idempotent, so this is safe even if the
		// refusal happened after the lease had already gone.
		e.destroyBackend(ctx, pc)
		return releaseVerdict{limb: releaseLimbHandback, detail: err.Error()}
	}
	return v
}

// destroyBackend closes the physical connection instead of letting the pool
// keep it.
//
// GIVING THE LEASE BACK IS NOT THE SAME AS CLOSING THE SOCKET, and this cost a
// live cell to discover. The driver's ordinary discard relinquishes the lease
// and then decides for itself whether the member can be reused, and that
// decision is made on wire mechanics alone: nothing in flight, nothing
// poisoned, no transaction the driver itself opened. A backend whose reset the
// server just refused passes every one of those tests — the wire is in perfect
// order and the session state is exactly what we failed to clear — so the
// connection would go straight back into the pool, which is the outcome this
// whole file exists to prevent.
//
// THE DRIVER CANNOT BE BLAMED FOR THAT: session state is not something it can
// see. It is autodb that holds the fact, so autodb has to state it, and the
// driver offers an operation that takes the demand literally — poison under the
// lock, interrupt and barrier behind any in-flight read or write, close the
// socket, give the lease back — with no appeal to whether the wire looks
// reusable. That operation is what this function calls.
//
// THE OBVIOUS ALTERNATIVE IS WRONG, AND IT IS WHAT THIS REPLACED. A caller can
// arrange a wire state the driver's reuse test rejects — queue a frame, never
// flush it — and let the driver reach the right conclusion for the wrong
// reason. That couples a security property of this product to an unexported
// predicate in a library: the day that predicate is widened for a perfectly
// good reason, a failed reset silently becomes a reuse of contaminated state,
// with nothing to compile against and no test anywhere that notices.
//
// THERE IS NO FALLBACK, AND THAT IS THE CHANGE. The weaker teardown used to
// live here for a driver without the capability. It is gone, because a
// teardown is the wrong place to discover that this install cannot honour its
// isolation guarantee: by then the session has already run the client's work
// on a backend nobody can destroy. pinTargetBackend asserts the capability
// when the member is pinned and still unused, so by the time any backend
// reaches this function it is destructible. A false answer below is therefore
// not an older driver — it is that invariant being broken, which is a defect
// in this package and is logged as one rather than quietly absorbed.
func (e *Engine) destroyBackend(_ context.Context, pc golibpg.PinnedConn) {
	if d, ok := pc.(golibpg.Destroyer); ok {
		d.Destroy()
		return
	}
	// THE DEFENSIVE MISS DOES NOT HAND THE BACKEND BACK, AND THAT IS A CHANGE.
	//
	// It used to Discard here, on the reasoning that stranding a pool member
	// forever is worse than one the pool may recycle. That reasoning is wrong
	// for THIS member. Everything reaching this function is a backend a
	// session has USED and whose reset could not be proved, so what Discard
	// risks is not an idle slot: it is the driver's own reuse test deciding
	// that a backend carrying another developer's session state is fit to
	// serve the next one. That is the leak the whole release gate exists to
	// prevent, reintroduced on the one path nobody expected to run.
	//
	// So it costs a pool slot instead, and says so loudly. The slot is
	// recoverable by restarting; the contaminated session that would have been
	// handed to the next caller is not.
	//
	// UNREACHABLE unless pinTargetBackend's boundary assertion has been
	// removed, which is why this is logged as a defect in this package rather
	// than as an older driver being coped with.
	e.logf("BUG: a pinned backend reached destruction without the capability that " +
		"pinTargetBackend requires of every pin. Its lease is NOT being returned: its " +
		"session state could not be cleared, and handing it back would let the driver's " +
		"own reuse test give another caller's state to the next request. One pool slot " +
		"is held until restart")
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

// backendResetRunner is the whole of what the reset plan needs from a backend:
// run one statement on it, and report separately the three things that decide
// the verdict — the transport failing, the target refusing, and the transaction
// status the backend reports afterwards.
//
// It is an interface because the plan has to run on two backends that have
// nothing else in common: a pinned connection's simple-query face, and the raw
// connection the driver's pool hands to its release hook. Collapsing those into
// one type is not possible; writing the plan twice is, and is exactly what must
// not happen — see the header of this file. So the transport varies and the
// plan, the order, the timeout, the limb names and the verdict do not.
type backendResetRunner interface {
	// runResetStatement carries one step and returns the backend's transaction
	// status, the server's refusal if it refused, and a transport error if the
	// wire itself failed. A server refusal is protocol data, not a Go error:
	// the two mean different things to the verdict and must not be merged.
	//
	// It takes the whole step rather than its text because a transport may have
	// to carry a step DIFFERENTLY — see resetStep.invalidatesDriverCache — and
	// the decision of how to carry it belongs to the transport, not to the plan.
	runResetStatement(ctx context.Context, step resetStep) (status byte, targetErr *pgconn.PgError, err error)
}

// runBackendReset runs the plan and reports the first step that could not be
// completed. It is shared by the two moments a backend's cleanliness has to be
// established on a PINNED connection — when a session gives one up, and when a
// session takes one — so that "clean" cannot come to mean two different things.
func (e *Engine) runBackendReset(ctx context.Context, pc golibpg.PinnedConn) releaseVerdict {
	sq, ferr := rawFace(pc)
	if ferr != nil {
		// Without a simple-query face there is no way to run a reset at all,
		// so there is no way to prove anything. That is a destroy, not an
		// exemption.
		return releaseVerdict{limb: releaseLimbFace, detail: ferr.Error()}
	}
	return e.runResetPlan(ctx, pinnedResetRunner{e: e, sq: sq})
}

// runResetPlan is THE reset. Every backend autodb sanitizes goes through this
// loop, whichever transport carried it.
//
// The context is detached from the caller's before the timeout is applied.
// A reset runs while a session is closing or while the driver is taking a
// connection back, and in both cases the work that owned the context is already
// over or already cancelled; inheriting that cancellation would abandon the
// reset halfway and discard a backend for a reason that has nothing to do with
// the backend.
func (e *Engine) runResetPlan(ctx context.Context, r backendResetRunner) releaseVerdict {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backendResetTimeout)
	defer cancel()
	for _, step := range e.resetPlan() {
		status, targetErr, derr := r.runResetStatement(rctx, step)
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

// pinnedResetRunner carries the plan over a pinned connection's simple-query
// face, which is the only face that exists while a session owns the wire.
type pinnedResetRunner struct {
	e  *Engine
	sq golibpg.SimpleQuerier
}

// runResetStatement carries every step the same way. The pinned face never runs
// a statement through pgx's high-level machinery, so no driver-side cache can be
// left disagreeing with the server and the step flag has nothing to change here.
func (r pinnedResetRunner) runResetStatement(ctx context.Context, step resetStep) (byte, *pgconn.PgError, error) {
	var targetErr *pgconn.PgError
	status, err := r.e.sessionSimpleQuery(ctx, r.sq, sessionSQLAutodb, step.sql,
		func(m golibpg.ExtendedMessage) error {
			if m.Kind == "ErrorResponse" && targetErr == nil {
				targetErr = m.Err
			}
			return nil
		})
	if err != nil {
		return 0, nil, err
	}
	return status, targetErr, nil
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
// door shares with everything else autodb runs. A backend reaches a session
// from a pool that also serves ordinary statements, connections that were idle
// across a configuration change, and connections the driver established while
// this process was doing something else entirely. A live cell in this package
// demonstrates the inheritance directly.
//
// THE RELEASE HOOK DOES NOT MAKE THIS REDUNDANT, which is worth saying because
// it looks like it should. The hook sanitizes a connection on its way back to
// the pool; it cannot speak for one that never went through a release this
// process saw, and a hook that failed silently would leave nothing between a
// dirty backend and a client session. Proving at checkout is the check that
// does not depend on any earlier check having run.
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
	e.destroyBackend(ctx, pc)
	return fmt.Errorf("exec: the backend taken for this session could not be proved clean (%s)", v.reason())
}
