package exec

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
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

// DemandDelivery says what the session's owner managed to tell its client
// before the session ended.
//
// TYPED, BECAUSE IT ENDS UP IN A RECORD SOMEBODY READS BACK. It was a bool and
// then a sentence; a bool cannot distinguish "we tried and the client had gone"
// from "we never got as far as trying", and a sentence invites a downstream
// regex nobody should be writing against an audit trail.
type DemandDelivery string

const (
	// DemandDelivered: the frame was written and flushed.
	DemandDelivered DemandDelivery = "delivered"
	// DemandFlushFailed: the frame was written and the client did not take it.
	// The session still ends -- holding its lease would punish everyone
	// waiting in order to protect somebody who is not listening.
	DemandFlushFailed DemandDelivery = "flush_failed"
	// DemandNotAttempted: nothing was sent, because something upstream of the
	// wire was wrong. The session still ends, and the record says the client
	// was never told rather than implying it refused to listen.
	DemandNotAttempted DemandDelivery = "not_attempted"
)

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
	// HeldObjects says whether the victim still had prepared statements or
	// portals on its backend when it was selected.
	//
	// IT CHANGES NOTHING ABOUT WHAT HAPPENS AND EVERYTHING ABOUT THE RECORD.
	// Both kinds terminate -- the scheduled unit is the wire lease, held for
	// the session's lifetime, so detaching a backend frees nothing for anyone
	// waiting, and an earlier design that kept the frontend session alive was
	// not reclamation at all. But the two are worth telling apart afterwards:
	// a target whose reclamations are mostly holders of objects is one where
	// clients are leaving statements open, which is a different operational
	// story from idle connections nobody closed.
	HeldObjects bool
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
		// THE DEMAND UNIT IS SPENT HERE, NOT WHERE IT WAS CHECKED.
		//
		// pressDemand's check is an optimisation and nothing more. Two receive
		// offers opening at once both read wanted=1/promised=0 before either
		// had recorded anything, both went on to select, and both reserved --
		// one queued request ending two healthy sessions. A check and a spend
		// in different critical sections is not accounting; it is a race with
		// bookkeeping attached.
		//
		// So the claim is taken atomically, under this candidate's own lock,
		// against the same state the reservation commits to. demandMu is a leaf
		// and nothing that touches a socket runs while it is held.
		if h := r.hookAtDemandClaim; h != nil && eligible {
			// AT THE CLAIM BOUNDARY, under this candidate's own lock. The
			// binding test convention requires a concurrency cell to FORCE the
			// window rather than hope for it, and this window is one statement
			// wide -- unreachable from outside the function.
			h(s.id)
		}
		claimed := eligible && r.tryPromiseDemand(leaseConn, s.id)
		// The reservation is taken INSIDE this same hold. It is the ordinary
		// close claim, so it also settles ownership against the reaper, an
		// operator's delete and the client's own disconnect.
		reserved := claimed && s.beginCloseLocked("", ReasonDemandReclaimed)
		if claimed && !reserved {
			// GIVEN BACK BEFORE MOVING ON. The close claim can be lost to the
			// reaper or the client's own disconnect; a demand unit left spent
			// on a candidate nobody reserved would make the target look
			// answered and strand the request that is still waiting.
			r.dischargeDemand(leaseConn, s.id)
		}
		var knock func()
		if reserved {
			// PUBLISHED UNDER THE SAME LOCK AS THE RESERVATION. The owner
			// cannot be woken about a session that was not reserved, and a
			// session cannot be reserved without its owner being told -- which
			// is what stopped a lost race from ending somebody's session
			// silently.
			// ISSUED WITH THE RESERVATION, so exactly one finalisation exists
			// for exactly one reservation. The demand unit was already spent
			// above, atomically, which is what makes this reservation the only
			// one that could have happened.
			s.demandFinal = true
			s.demandIdle = now.Sub(s.lastUsed)
			s.demandHeldObjects = s.holdsObjects()
			s.pendingNotice = &DemandNotice{
				Gen: s.gen, ID: s.id, IdleFor: s.demandIdle,
				HeldObjects: s.demandHeldObjects,
			}
			knock = s.wake
			// RECORDED IN THE SAME HOLD AS THE RESERVATION, through the leaf,
			// so no second ask can be decided against a view in which this one
			// has not happened yet. That window is exactly where over-reclaim
			// would live.
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
	if s.wake == nil {
		// NO OFFER WITHOUT A KNOCK. An offer says "I am blocked reading and can
		// be told"; without a registered knock nothing can make that read
		// return, so the offer would be a promise this session cannot keep --
		// and the cost of believing it is a lease held forever by a session
		// that has been closed and can serve nobody.
		s.mu.Unlock()
		return 0
	}
	s.tokenSeq++
	s.recvToken = s.tokenSeq
	token := s.recvToken
	target := s.reservation.LeaseConn
	s.mu.Unlock()

	// THIS IS THE INSTANT A HOLDER BECOMES ASKABLE, and therefore the only
	// event that can answer a request that arrived when nothing was.
	//
	// A lease is held for a session's whole lifetime, so on a full target
	// nothing releases by itself: if the arrival ask found every holder busy,
	// transacting or between offers, no further ask would ever be made and the
	// queued request would wait out its whole bound. Publishing an offer is
	// precisely the transition from unaskable to askable, so the ask belongs
	// here -- and being an event, it costs nothing when nobody is waiting.
	//
	// OUTSIDE THIS SESSION'S LOCK. Selecting a victim takes the registry mutex
	// and then each candidate's, this session among them.
	//
	// pressDemand asks only while more requests are waiting than reclamations
	// are already coming, so a target with one waiter and several idle holders
	// gives up one lease, not several.
	e.sessions.pressDemand(target)

	return token
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
func (e *Engine) FinishDemandReclaim(ctx context.Context, id SessionID, gen uint64, delivery DemandDelivery) bool {
	s, ok := e.demandTarget(id, gen)
	if !ok {
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
	if !e.claimDemandFinalisation(s, delivery) {
		// SOMEBODY ELSE OWNS THIS ENDING, or this caller has already finalised
		// it. Returning here is what keeps one ending to one teardown, one
		// audit line and one lease release.
		return false
	}

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

// demandTarget resolves a notice to the session it was actually about.
//
// SEPARATED SO THE REFUSAL CAN BE TESTED WITHOUT A TEARDOWN. The guarantee here
// is about identity, not about closing: a notice whose generation no longer
// matches is about a session that has already ended, and honouring it would end
// whichever session came after it -- disconnecting somebody who was never
// selected. Proving that through FinishDemandReclaim meant letting the failure
// case run into the teardown, where a cell either needs a whole Engine or
// panics; a panic proves nothing about the claim, which is why the runner
// classifies one as INVALID rather than RED.
func (e *Engine) demandTarget(id SessionID, gen uint64) (*session, bool) {
	s, ok := e.sessions.byIDOnly(id)
	if !ok || s.gen != gen {
		return nil, false
	}
	return s, true
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
// claimDemandFinalisation consumes this session's finalisation claim and
// records the outcome, or refuses.
//
// VALIDATED AND CONSUMED UNDER ONE HOLD, because a check and a claim in two
// holds is the same race in a smaller window: both callers would see the claim
// intact, and both would take it. Everything that decides is read here --
// whether a claim is outstanding, whether the session is actually closing, and
// whether it is closing for THIS reason -- so a caller that arrives against an
// ordinary close, or a second time, is refused rather than allowed to write a
// reclamation over somebody else's ending.
func (e *Engine) claimDemandFinalisation(s *session, delivery DemandDelivery) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.demandFinal || s.get() != sessClosing ||
		!strings.HasPrefix(s.closeWhy, ReasonDemandReclaimed) {
		return false
	}
	s.demandFinal = false
	// The idle time is the one recorded AT SELECTION, not measured again now.
	// See session.demandIdle.
	s.closeWhy = ReasonDemandReclaimed + demandOutcomeSuffix(s.demandIdle, s.demandHeldObjects, delivery)
	return true
}

// holdsObjects reports whether anything of this session's lives on its backend.
// Caller holds s.mu.
//
// PENDING CLOSES COUNT AS PRESENT: a Close that is queued but not acknowledged
// means the target may still hold the object, so the store is not yet empty.
func (s *session) holdsObjects() bool {
	if s.ext == nil {
		return false
	}
	return len(s.ext.statements) > 0 || len(s.ext.portals) > 0 || len(s.ext.pendingCloses) > 0
}

// demandOutcomeSuffix completes the close reason with what only the owner knew.
// demandOutcomeSuffix serialises what only the owner knew, as DATA.
//
// CANONICAL JSON, NOT PROSE. The audit schema carries one detail string, and
// the previous version filled it with an English sentence -- which reads well
// and cannot be queried, so anything downstream that wanted the idle time or
// the delivery outcome would have had to regex an audit trail. The reason
// itself stays a stable identity and is never parsed; everything variable is a
// field beside it.
func demandOutcomeSuffix(idle time.Duration, held bool, delivery DemandDelivery) string {
	state := "clean"
	if held {
		state = "holds_objects"
	}
	payload, err := json.Marshal(struct {
		ReclaimState   string         `json:"reclaim_state"`
		IdleMS         int64          `json:"idle_ms"`
		ClientDelivery DemandDelivery `json:"client_delivery"`
	}{state, idle.Milliseconds(), delivery})
	if err != nil {
		// UNREACHABLE for a struct of two strings and an integer, and it
		// degrades rather than losing the record: the stable reason above it
		// is what an operator counts, and it is already written.
		return " {}"
	}
	return " " + string(payload)
}

// -- Demand accounting -------------------------------------------------------
//
// All four take demandMu and NOTHING ELSE, so any of them may be called from
// under the registry mutex or from under a session's, which is the whole
// reason this is a separate leaf rather than more state behind r.mu.

// wantDemand records that one more queued request on this target needs a lease
// reclaimed.
func (r *sessionRegistry) wantDemand(leaseConn int64) {
	if leaseConn == 0 {
		return
	}
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	if r.demandWanted == nil {
		r.demandWanted = map[int64]int{}
	}
	r.demandWanted[leaseConn]++
}

// dropDemand records that a queued request no longer needs one, whether it was
// served, cancelled, expired or refused.
func (r *sessionRegistry) dropDemand(leaseConn int64) {
	if leaseConn == 0 {
		return
	}
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	if n := r.demandWanted[leaseConn]; n > 1 {
		r.demandWanted[leaseConn] = n - 1
	} else {
		// Deleted rather than left at zero: a map of every target that ever
		// queued a request grows without bound on a long-lived daemon.
		delete(r.demandWanted, leaseConn)
	}
}

// tryPromiseDemand spends one demand unit on this session, or refuses.
//
// CHECKING AND SPENDING ARE THE SAME OPERATION, which is the whole point of it
// returning a bool. Reading "is one still owed" and recording "this one is
// answering it" in two critical sections lets every concurrent offer read the
// same encouraging answer and act on it, which is how one waiting request came
// to end several sessions.
func (r *sessionRegistry) tryPromiseDemand(leaseConn int64, id SessionID) bool {
	if leaseConn == 0 {
		return false
	}
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	if r.demandWanted[leaseConn] <= len(r.demandPromised[leaseConn]) {
		return false
	}
	if r.demandPromised == nil {
		r.demandPromised = map[int64]map[SessionID]struct{}{}
	}
	if r.demandPromised[leaseConn] == nil {
		r.demandPromised[leaseConn] = map[SessionID]struct{}{}
	}
	r.demandPromised[leaseConn][id] = struct{}{}
	return true
}

// dischargeDemand records that a reserved session's lease has come back.
func (r *sessionRegistry) dischargeDemand(leaseConn int64, id SessionID) {
	if leaseConn == 0 {
		return
	}
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	m := r.demandPromised[leaseConn]
	if m == nil {
		return
	}
	delete(m, id)
	if len(m) == 0 {
		delete(r.demandPromised, leaseConn)
	}
}

// demandOutstanding reports whether more requests are waiting on this target
// than reclamations are already coming for it.
func (r *sessionRegistry) demandOutstanding(leaseConn int64) bool {
	if leaseConn == 0 {
		return false
	}
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	return r.demandWanted[leaseConn] > len(r.demandPromised[leaseConn])
}

// pressDemand asks for one reclamation on this target, but only while one is
// still owed.
//
// EVERY ASK GOES THROUGH HERE, the arrival and the retry alike, so there is
// one place that decides whether asking is warranted. Called with no lock
// held: selecting a victim reads sessions and knocks on a socket.
func (r *sessionRegistry) pressDemand(leaseConn int64) bool {
	// AN OPTIMISATION, NOT THE ACCOUNTING. It spares a pointless walk over the
	// candidates when nothing is owed. Correctness rests on tryPromiseDemand at
	// the reservation itself -- this answer is stale the instant it is read.
	if r == nil || !r.demandOutstanding(leaseConn) {
		return false
	}
	r.mu.Lock()
	demand := r.onDemand
	r.mu.Unlock()
	if demand == nil {
		return false
	}
	return demand(leaseConn)
}

// promisedCount reports how many reclamations are already coming for a target.
// Test-support: the accounting is the guarantee, so a cell has to be able to
// read it rather than infer it from how many sessions happen to be closing.
func (r *sessionRegistry) promisedCount(leaseConn int64) int {
	r.demandMu.Lock()
	defer r.demandMu.Unlock()
	return len(r.demandPromised[leaseConn])
}
