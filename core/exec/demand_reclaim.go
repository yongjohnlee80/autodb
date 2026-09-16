package exec

import (
	"context"
	"sync/atomic"
	"time"
)

// WHEN NOBODY IS GOING TO RELEASE, SOMEBODY IS ASKED TO LEAVE.
//
// The queue answers contention that clears on its own: a colleague's session
// ends, the lease moves to the longest-waiting request, nobody notices. It is
// no answer at all when every lease is held by a session that is simply SITTING
// there — a developer who opened a connection this morning and has not typed
// since. Those sessions release nothing, so the line drains only by expiring,
// and a target can stay unusable for as long as its idle holders stay logged
// in.
//
// SO AN IDLE HOLDER IS ENDED, AND IS TOLD SO. That is a real cost to a real
// person and the design pays it deliberately rather than quietly: the client
// receives the agreed fatal frame naming what happened and what to do about it,
// BEFORE anything of theirs is torn down. A session that is merely disconnected
// leaves a developer guessing, which is the failure this whole body of work
// exists to stop.
//
// THIS PACKAGE NEVER WRITES TO THE WIRE. The client's connection has exactly
// one owner — the front door's session loop, which is blocked reading from it —
// and a second writer would interleave bytes into a protocol stream mid-frame.
// So the scheduler's whole job here is to CLAIM a victim and wake its owner.
// The frame, the flush, the acknowledgement and the teardown all happen on the
// loop, in that order, and the lease is not released until the client has been
// told. See frontdoor/session_loop.go.

// terminalClaim is the single-use right to end one session, bound to the
// generation it was taken at.
//
// SINGLE USE IS WHAT STOPS A DOUBLE ANSWER. Demand, the client's own
// disconnect, an operator's connection delete and the idle reaper can all
// decide to end the same session within the same instant. Exactly one may
// frame it, audit it and release its lease; the others must find the claim
// taken and do nothing. A second terminal frame on a closing connection is at
// best noise and at worst a write into a socket another goroutine is closing.
//
// THE GENERATION IS WHAT STOPS A CLAIM KILLING A STRANGER. A claim taken
// against a session that ends on its own before the wake arrives must not be
// honoured by whatever session next occupies that connection: the loop
// re-checks the generation it holds against the session's current one and
// declines a mismatch.
type terminalClaim struct {
	// taken is the claim itself. Compare-and-swapped, so the winner is decided
	// by the hardware rather than by a lock this path would otherwise have to
	// take while holding the registry's.
	taken atomic.Bool
	gen   atomic.Uint64
}

// claim takes the terminal right for this session, returning the generation the
// holder must present, and whether it was taken.
func (c *terminalClaim) claim() (uint64, bool) {
	if !c.taken.CompareAndSwap(false, true) {
		return 0, false
	}
	return c.gen.Load(), true
}

// valid reports whether a presented generation still matches.
func (c *terminalClaim) valid(gen uint64) bool { return c.taken.Load() && c.gen.Load() == gen }

// DemandNotice is what the scheduler hands the session's owner. Exported
// because the owner is the front door, in another package: this package decides
// WHO gives up a lease, and the front door's session loop is the only thing
// allowed to tell that client about it.
type DemandNotice struct {
	// Gen is the generation the claim was taken at; the owner presents it back
	// so a claim cannot outlive the session it was taken against.
	Gen uint64
	// IdleFor is how long the victim had been silent, for the audit line. The
	// number is the justification for ending somebody's session and belongs in
	// the record beside the decision.
	IdleFor time.Duration
}

// demandVictim is one selected idle holder and the claim taken on it.
type demandVictim struct {
	s      *session
	notice DemandNotice
}

// claimDemandVictim selects an idle lease holder on this target and takes the
// terminal claim on it, or reports none.
//
// SELECTED OUTSIDE THE REGISTRY LOCK, DELIBERATELY. Judging a session takes
// that session's own mutex, and taking it while holding the registry's would
// nest the two locks in the order that makes demand reclamation deadlock
// against everything else the registry does. So the candidates are snapshotted
// under the registry lock and judged under their own, which is the same shape
// every other cross-session count in this package uses.
//
// THE LONGEST-SILENT HOLDER IS CHOSEN, not the first one found. Every candidate
// is equally reclaimable by the predicate, but they are not equally likely to
// be missed: the developer who has been away an hour is the one whose session
// costs the least to end, and choosing arbitrarily would sometimes end the
// session of somebody who paused for thirty seconds while an hour-idle one sat
// beside it.
func (r *sessionRegistry) claimDemandVictim(leaseConn int64, now time.Time) (demandVictim, bool) {
	r.mu.Lock()
	candidates := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		if s.reservation.LeaseConn == leaseConn {
			candidates = append(candidates, s)
		}
	}
	r.mu.Unlock()

	var best *session
	var bestIdle time.Duration
	for _, s := range candidates {
		s.mu.Lock()
		idle := now.Sub(s.lastUsed)
		// EVERY CONDITION IS REQUIRED, and each names somebody who would be
		// harmed rather than merely inconvenienced:
		//
		//   - not open        -> something already owns this teardown;
		//   - not a wire session -> there is no client loop to frame it, and an
		//     internal session has no one to tell;
		//   - a request in flight -> ending it cancels work that is inside its
		//     bounds, which the ruling forbids outright;
		//   - a transaction open -> ending it rolls back work the holder never
		//     abandoned, and they find out from their next statement.
		//
		// What is NOT required is an empty object store. A holder with prepared
		// statements or portals is precisely the holder whose backend cannot be
		// handed to anyone else, which is why the answer for them is a framed
		// ending rather than a silent handover: told what happened, they
		// reconnect and rebuild what they had.
		//   - no registered owner -> nothing can tell this client what
		//     happened, and ending a session silently is worse than not
		//     reclaiming it. Checked HERE rather than after the claim: a claim
		//     spent on a session nobody can frame is a claim wasted, and the
		//     request that triggered it waits the full bound for nothing.
		eligible := s.get() == sessOpen && s.wire && !s.busy && s.tx == nil && s.wake != nil
		s.mu.Unlock()
		if eligible && (best == nil || idle > bestIdle) {
			best, bestIdle = s, idle
		}
	}
	if best == nil {
		return demandVictim{}, false
	}
	gen, ok := best.terminal.claim()
	if !ok {
		// Something else took this teardown between the judgement and the
		// claim. Reporting none is correct and costs one wait: the caller is
		// already in line, and the release that other teardown performs will
		// serve the line exactly as this one would have.
		return demandVictim{}, false
	}
	return demandVictim{s: best, notice: DemandNotice{Gen: gen, IdleFor: bestIdle}}, true
}

// demandReclaim asks an idle holder on this target to give up its lease, and
// reports whether one was asked.
//
// IT RETURNS AS SOON AS THE OWNER IS WOKEN, not when the lease is free. The
// waiting is already handled: the caller is in line, and when the owner has
// framed its client and torn down, the release serves the line in arrival
// order. Blocking here would also let one request's demand hold the scheduler
// open for however long a client takes to accept a frame.
//
// THE FREED LEASE IS NOT THE CALLER'S. It goes to the longest-waiting request
// that can use it, which may well be somebody else. Handing it to whoever
// triggered the reclaim would make demand a way to jump the queue, and a queue
// with a bypass is not a queue.
func (e *Engine) demandReclaim(leaseConn int64) bool {
	if e.sessions == nil {
		return false
	}
	v, ok := e.sessions.claimDemandVictim(leaseConn, e.now())
	if !ok {
		return false
	}
	wake := v.s.takeWake()
	if wake == nil {
		// Claimed, but nothing can reach its client: the owner has already
		// gone. Leaving the claim taken is correct — it stops anything else
		// trying to frame a client nobody owns — and the session's own teardown
		// releases the lease.
		e.logf("connection %d: session %s was selected for demand reclamation but has no "+
			"live owner to frame it; leaving its teardown to the owner that took it",
			v.s.connID, v.s.id)
		return false
	}
	wake(v.notice)
	return true
}

// registerWake publishes the callback the front door's session loop listens on.
// Called once, by the owner, at session open.
func (s *session) registerWake(f func(DemandNotice)) {
	s.mu.Lock()
	s.wake = f
	s.mu.Unlock()
}

// takeWake reads the owner's wake callback.
func (s *session) takeWake() func(DemandNotice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

// FinishDemandReclaim releases a demand-reclaimed session once its owner has
// told the client. Called by the front door's session loop, and only after the
// fatal frame has been flushed.
//
// THE ORDER IS THE CONTRACT. Releasing before the client is told would let the
// lease reach a new session — and the new session's first statement could reach
// the target — while the old client still believes it holds a connection and
// has been given no reason to think otherwise.
func (e *Engine) FinishDemandReclaim(ctx context.Context, id SessionID, gen uint64) bool {
	s, ok := e.sessions.byIDOnly(id)
	if !ok || !s.terminal.valid(gen) {
		// A generation that no longer matches is a claim against a session that
		// has already ended. Declining is what stops it from ending whichever
		// session came after it.
		return false
	}
	e.closeSession(ctx, s, "", ReasonDemandReclaimed)
	return true
}

// ReasonDemandReclaimed is the audit identity for a session ended so its lease
// could serve a waiting request. ONE identity, written once by the teardown
// that owns the claim, so the record cannot say a session was ended twice for
// the same reason.
const ReasonDemandReclaimed = "demand-reclaimed"

// byIDOnly looks a session up without the owner check the caller-facing lookup
// applies. Used by the teardown path, which has already proved its right to act
// through the terminal claim rather than through a caller's identity.
func (r *sessionRegistry) byIDOnly(id SessionID) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

// RegisterDemandWake publishes the callback the session's owner listens on.
//
// CALLED ONCE, BY THE OWNER, AT SESSION OPEN. The callback is how this package
// reaches the one goroutine permitted to write to that client's socket; there
// is deliberately no other route, because a second writer would interleave
// bytes into a protocol stream mid-frame.
//
// A session with no registered wake is simply never selected to give up its
// lease: demand reclamation that cannot tell the client what happened does not
// happen at all. That is the same fail-closed shape as the rest of this path --
// ending somebody's session silently is worse than not reclaiming.
func (e *Engine) RegisterDemandWake(id SessionID, f func(DemandNotice)) {
	s, ok := e.sessions.byIDOnly(id)
	if !ok {
		return
	}
	s.registerWake(f)
}
