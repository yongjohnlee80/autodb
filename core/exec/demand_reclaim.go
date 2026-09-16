package exec

import (
	"context"
	"sort"
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
// BEFORE anything of theirs is torn down.
//
// THIS PACKAGE NEVER WRITES TO THE WIRE. The client's connection has exactly
// one owner — the front door's session loop, which is blocked reading from it —
// and a second writer would interleave bytes into a protocol stream mid-frame.
// So the scheduler reserves a victim and offers the notice to that owner. The
// frame, the flush and the release all happen on the loop, in that order.
//
// THE RESERVATION IS THE ORDINARY CLOSE STATE MACHINE, NOT A CLAIM BESIDE IT.
// An earlier version kept its own single-use flag, which left two independent
// answers to "who is ending this session": demand could reserve a session that
// the reaper, an operator's connection delete, or the client's own disconnect
// was already ending, and neither knew about the other. Worse, the flag stopped
// nothing — a query arriving after selection would run, and the session would
// be terminated after becoming active. beginClose moves the session to closing,
// which every dispatch path already refuses to serve, so reserving it both
// settles ownership and stops new work in one compare-and-swap.

// DemandNotice is what the scheduler offers the session's owner. Exported
// because the owner is the front door, in another package: this package decides
// WHO gives up a lease, and the front door's session loop is the only thing
// allowed to tell that client about it.
type DemandNotice struct {
	// Gen is the generation of the session this notice is about, so a notice
	// cannot be honoured against whichever session next occupies its place.
	Gen uint64
	// ID names that session, for the finalisation the owner performs.
	ID SessionID
	// IdleFor is how long the victim had been silent. The number is the
	// justification for ending somebody's session and belongs in the record
	// beside the decision.
	IdleFor time.Duration
}

// demandVictim is one reserved session and the notice offered for it.
type demandVictim struct {
	s      *session
	notice DemandNotice
}

// reserveDemandVictim finds an idle lease holder on this target and reserves it
// for termination, or reports none.
//
// CANDIDATES ARE JUDGED AND RESERVED UNDER THE SAME LOCK HOLD. The predicate
// and the reservation used to be separated by an unlock, and a frame or a BEGIN
// arriving in that gap meant a session could be selected while idle and
// terminated while active. Here the decision and the claim are one critical
// section per candidate, so a session that becomes busy simply fails the check.
//
// A CANDIDATE THAT LOSES THE RESERVATION COSTS NOTHING. If something else is
// already ending that session, the next candidate is tried; nothing has been
// spent on the one that got away.
func (r *sessionRegistry) reserveDemandVictim(leaseConn int64, now time.Time) (demandVictim, bool) {
	r.mu.Lock()
	candidates := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		if s.reservation.LeaseConn == leaseConn {
			candidates = append(candidates, s)
		}
	}
	r.mu.Unlock()

	// THE LONGEST-SILENT HOLDER IS TRIED FIRST. Every candidate is equally
	// reclaimable by the predicate, but they are not equally cheap to end:
	// choosing arbitrarily would sometimes end the session of somebody who
	// paused for a moment while an hour-idle one sat beside it. Read outside
	// the registry lock, and only as an ordering hint — the value that decides
	// anything is re-read under each session's own lock below.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].idleFor(now) > candidates[j].idleFor(now)
	})

	for _, s := range candidates {
		s.mu.Lock()
		// EVERY CONDITION IS REQUIRED, and each names somebody who would be
		// harmed rather than merely inconvenienced:
		//
		//   - not a wire session -> there is no client loop to frame it;
		//   - a request in flight -> ending it cancels work inside its bounds,
		//     which the ruling forbids outright;
		//   - a transaction open -> ending it rolls back work the holder never
		//     abandoned, and they find out from their next statement;
		//   - not offering a receive epoch -> the owner is not blocked reading
		//     from its client, so a notice posted now would be overwritten by
		//     the loop re-arming its own deadline and would go dormant. The
		//     lease would stay held and the waiting request would wait its
		//     whole bound for a reclamation that had in fact been decided.
		//
		// What is NOT required is an empty object store. A holder with prepared
		// statements or portals is precisely the holder whose backend cannot be
		// handed to anyone else, which is why the answer for them is a framed
		// ending rather than a silent handover.
		//   - no knock registered -> there is nothing to make the owner's read
		//     return, so the notice would sit unread and the lease would be
		//     stranded by a session that is closing and serves nobody. Checked
		//     here AS WELL AS in OfferReceive, because the two guard different
		//     mistakes: that one stops a live offer existing without a knock,
		//     and this one stops a reservation committing against one anyway.
		eligible := s.wire && !s.busy && s.tx == nil && s.recvToken != 0 && s.wake != nil
		// The reservation is taken INSIDE this same hold. It is the ordinary
		// close claim, so it also settles ownership against the reaper, an
		// operator's delete and the client's own disconnect.
		reserved := eligible && s.beginCloseLocked("", ReasonDemandReclaimed)
		var knock func()
		if reserved {
			// PUBLISHED UNDER THE SAME LOCK AS THE RESERVATION. The owner
			// cannot be woken about a session that was not reserved, and a
			// session cannot be reserved without its owner being told -- which
			// is what stopped a lost race from ending somebody's session
			// silently.
			s.demandIdle = now.Sub(s.lastUsed)
			s.pendingNotice = &DemandNotice{
				Gen: s.gen, ID: s.id, IdleFor: s.demandIdle,
			}
			knock = s.wake
		}
		notice := s.pendingNotice
		s.mu.Unlock()

		if reserved {
			if knock != nil {
				// Outside the lock: the knock touches the client's connection,
				// and nothing that touches a socket runs under this mutex.
				knock()
			}
			return demandVictim{s: s, notice: *notice}, true
		}
	}
	return demandVictim{}, false
}

// demandReclaim asks an idle holder on this target to give up its lease, and
// reports whether one was asked.
//
// IT RETURNS AS SOON AS THE OWNER HAS ACCEPTED, not when the lease is free. The
// waiting is already handled: the caller is in line, and when the owner has
// framed its client and torn down, the release serves the line in arrival
// order. Blocking here would let one request's demand hold the scheduler open
// for however long a client takes to accept a frame.
//
// THE FREED LEASE IS NOT THE CALLER'S. It goes to the longest-waiting request
// that can use it, which may well be somebody else. Handing it to whoever
// triggered the reclaim would make demand a way to jump the queue, and a queue
// with a bypass is not a queue.
func (e *Engine) demandReclaim(leaseConn int64) bool {
	if e.sessions == nil {
		return false
	}
	v, ok := e.sessions.reserveDemandVictim(leaseConn, e.now())
	if !ok {
		return false
	}

	// The owner has it: reserving and telling it were the same operation. It
	// will frame its client and then finalise, which is what releases the lease.
	_ = v
	return true
}

// OfferReceive opens this session's receive offer and returns the token its
// owner must present to close it. Called immediately before the owner blocks
// reading from its client.
func (e *Engine) OfferReceive(id SessionID) uint64 {
	s, ok := e.sessions.byIDOnly(id)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		// NO OFFER WITHOUT A KNOCK. An offer says "I am blocked reading and can
		// be told"; without a registered knock nothing can make that read
		// return, so the offer would be a promise this session cannot keep --
		// and the cost of believing it is a lease held forever by a session
		// that has been closed and can serve nobody.
		return 0
	}
	s.tokenSeq++
	s.recvToken = s.tokenSeq
	return s.recvToken
}

// RetireReceive closes the offer and hands back any notice published inside it.
//
// CALLED AS THE READ RETURNS, BEFORE THE FRAME OR THE ERROR IS LOOKED AT. Both
// a delivered frame and a knock bring the owner back here; taking the notice
// first is what makes the winner explicit, because a frame belonging to a
// session already reserved for termination must not be dispatched.
//
// A STALE TOKEN RETIRES NOTHING. It would mean this call belongs to an offer
// that has already been closed, and honouring it would let one read's outcome
// close a later read's window.
func (e *Engine) RetireReceive(id SessionID, token uint64) (DemandNotice, bool) {
	s, ok := e.sessions.byIDOnly(id)
	if !ok || token == 0 {
		return DemandNotice{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recvToken != token {
		return DemandNotice{}, false
	}
	s.recvToken = 0
	n := s.pendingNotice
	s.pendingNotice = nil
	if n == nil {
		return DemandNotice{}, false
	}
	return *n, true
}

// RegisterDemandWake publishes the knock that makes this session's owner return
// from its blocked read. Called once, by the owner, at session open.
//
// A session whose owner is not currently offering to receive is never selected,
// which OfferReceive and RetireReceive decide. Ending a session nobody can
// explain it to is worse than not reclaiming it.
func (e *Engine) RegisterDemandWake(id SessionID, knock func()) {
	s, ok := e.sessions.byIDOnly(id)
	if !ok {
		return
	}
	s.mu.Lock()
	s.wake = knock
	s.mu.Unlock()
}

// FinishDemandReclaim completes a reclamation the owner has now told its client
// about, and reports whether THIS reservation owned the teardown.
//
// THE ORDER IS THE CONTRACT. Releasing before the client is told would let the
// lease reach a new session -- whose first statement could reach the target --
// while the old client still believes it holds a connection and has been given
// no reason to think otherwise. So the owner calls this only after its frame
// has been flushed.
//
// THE RETURN VALUE IS NOT DECORATIVE. A caller that ignored it could record a
// reclamation that another path had already performed, which is how one ending
// becomes two entries in the trail and two releases of one lease.
func (e *Engine) FinishDemandReclaim(ctx context.Context, id SessionID, gen uint64, delivered bool) bool {
	s, ok := e.sessions.byIDOnly(id)
	if !ok || s.gen != gen {
		// A generation that no longer matches is a notice about a session that
		// has already ended. Declining is what stops it ending whichever
		// session came after it.
		return false
	}
	// THE ONE RECORD OF THIS ENDING IS THE CLOSE'S OWN, AND IT IS COMPLETED
	// HERE RATHER THAN DUPLICATED.
	//
	// The teardown already writes a session_closed line carrying this
	// session's close reason, which is what a reclamation IS. The front door
	// used to write a second line of its own beside it, so one ending produced
	// two entries -- anyone counting reclamations counted them twice, and the
	// two could disagree about what happened. The two facts only the owner
	// knows, how long the session had been silent and whether its client
	// actually received the frame, are folded into that single reason instead.
	// The idle time is the justification for ending somebody's session and the
	// delivery flag is whether they were told; both belong in the record beside
	// the decision, not in a record of their own.
	e.completeDemandReason(s, delivered)

	// NOT PRE-CLAIMED. The teardown slot is claimed by quiesce, inside
	// finishClosing, and claiming it here first made this path wait on itself:
	// the claim creates the channel that the join then waits on, and only the
	// deferred release closes it, so every successful reclamation sat out the
	// full quiesce bound before releasing the lease it had just freed. The
	// reservation this caller already holds is what proves its right to act;
	// the slot is quiesce's to take.
	e.finishClosing(ctx, s)
	return true
}

// ReasonDemandReclaimed is the audit identity for a session ended so its lease
// could serve a waiting request.
const ReasonDemandReclaimed = "demand-reclaimed"

// byIDOnly looks a session up without the owner check the caller-facing lookup
// applies, and without its open-state requirement -- a session being ended is
// exactly the one this path needs to find. It has already proved its right to
// act by holding the reservation.
func (r *sessionRegistry) byIDOnly(id SessionID) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

// completeDemandReason folds what only the owner knew into the one record this
// ending will leave.
func (e *Engine) completeDemandReason(s *session, delivered bool) {
	s.mu.Lock()
	// The idle time is the one recorded AT SELECTION, not measured again now.
	// See session.demandIdle.
	s.closeWhy = ReasonDemandReclaimed + demandOutcomeSuffix(s.demandIdle, delivered)
	s.mu.Unlock()
}

// demandOutcomeSuffix completes the close reason with what only the owner knew.
func demandOutcomeSuffix(idle time.Duration, delivered bool) string {
	told := "the client was told"
	if !delivered {
		told = "the client could not be told: the connection was already gone"
	}
	return " (idle " + idle.Round(time.Second).String() + "; " + told + ")"
}
