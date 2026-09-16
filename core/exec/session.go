package exec

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// ExecSession — an engine-owned client session.
//
// The engine had no per-client object at all. A token is not one: browser tabs
// share it, and one RPC connection runs eight concurrent handlers under it. A
// transport connection is not one either — rpc is a mechanical projection and
// must not become the thing that owns state. So the session is engine-issued
// and opaque, and one session means one client editor context or one
// front-door wire connection.
//
// This file is the registry and the lifecycle. Pinned transactions arrive
// next; every statement here runs on the pool exactly as Execute does today,
// so what lands with the transactions is the transaction, not also the
// session machinery underneath it.
//
// LOCK ORDER, published because the ADR requires it and because getting it
// wrong is how this class of code deadlocks:
//
//	registry.mu  →  session.mu
//
// No registry lock is ever held across session I/O, and no session lock is
// ever held while taking the registry's. A close that must wait for an
// in-flight statement waits on a channel, holding neither.

// SessionID is an opaque, engine-issued session identifier.
//
// It is cryptographically random because it is an authorization-relevant
// name: possession of one is not authority — every call re-resolves the token
// and re-checks ownership — but a guessable id would still let a caller probe
// which sessions exist, and existence is information.
type SessionID string

// Session-layer errors.
var (
	// ErrSessionBusy reports a second statement on a session that is already
	// running one. Sessions serialize; they do not queue, so a
	// caller learns immediately rather than waiting behind work it cannot
	// see.
	ErrSessionBusy = errors.New("exec: the session is already running a statement")

	// ErrSessionNotFound reports a session that does not exist, is closed, or
	// belongs to someone else — deliberately the SAME error for all three.
	// Distinguishing them would turn the id space into an oracle for which
	// sessions exist and who owns them.
	ErrSessionNotFound = errors.New("exec: no such session")

	// ErrSessionCapExceeded reports a refusal to open another session. It
	// names WHICH cap was hit, because "try again later" and "ask an
	// operator to raise the limit" are different actions.
	ErrSessionCapExceeded = errors.New("exec: session-cap-exceeded")

	// ErrConnectionDraining reports a connection being deleted or closed. A
	// session cannot be opened on it, and the pool must not be recreated for
	// it.
	ErrConnectionDraining = errors.New("exec: the connection is shutting down")
)

// sessionState is the lifecycle position. It only ever moves forward.
type sessionState int32

const (
	sessOpen sessionState = iota
	sessClosing
	sessClosed
)

func (s sessionState) String() string {
	switch s {
	case sessOpen:
		return "open"
	case sessClosing:
		return "closing"
	case sessClosed:
		return "closed"
	}
	return fmt.Sprintf("sessionState(%d)", int(s))
}

// session is one open client session.
type session struct {
	id     SessionID
	userID int64
	connID int64
	// authority names the durable row this exec session's authority rests
	// on. A pinned transaction outlives the call that opened it, so the
	// authority has to be re-checkable later without a token — and
	// revocation and expiry live on that row.
	//
	// TYPED, because there are two kinds and the previous shape could not
	// say which. It was a bare session id, and a front-door session — whose
	// authority is a PAT, not a session — stored a zero as a sentinel for
	// "no session row". The janitor passed that zero to a session-keyed
	// lookup, the missing row read as a revocation, and every wire
	// transaction would have been rolled back and closed on the first sweep,
	// audited as though permission had been withdrawn. Nothing caught it
	// because the two halves were tested separately: sweeps used token
	// sessions, wire sessions never opened a transaction.
	authority auth.AuthorityRef

	// demoted records that this session lost write privilege while it was
	// open. It is set by the sweep and read by the execution path, which
	// must not serve a write for a session the sweep has already demoted —
	// the full reader read-only wrap is F3a's, and this is the flag it will
	// hang on rather than a second source of truth.
	demoted bool

	// appName is the client's accepted startup application_name (frontdoor §3.1,
	// matrix claim #session-audit): recorded on the session and stamped into every
	// exec/exec_result audit line for this session's units. Empty for token sessions.
	appName string
	// wire marks a front-door session (opened by OpenWireSessionWith); token
	// sessions leave it false. It selects the audit stamp, nothing else.
	wire bool

	// ctx outlives the RPC call that created the session, which is the whole
	// point: a COMMIT arriving in a LATER call has to operate on a live
	// transaction, so the session's context cannot be the opening caller's.
	// WithoutCancel keeps the values and drops the cancellation.
	ctx    context.Context
	cancel context.CancelFunc

	// state is atomic so the terminal transition can be CAS-owned: exactly
	// one closer performs the teardown, however many arrive.
	state atomic.Int32

	mu sync.Mutex
	// busy is the one-in-flight-statement gate.
	busy bool
	// runCancel stops the in-flight statement without ending the session.
	runCancel context.CancelFunc
	// tearingDown marks the slot as held by a teardown rather than by a
	// statement, so a refusal can say which it is.
	tearingDown bool
	// reservation is what this session holds from the registry: a wire
	// lease and a memory charge. Released with it, never separately.
	reservation reservation

	// closeIP and closeWhy are kept so a retried close audits the reason the
	// close actually had, rather than inventing one at retry time.
	closeIP     string
	closeWhy    string
	closeActive bool
	// closeRetryRequested closes the handoff race where a demotion asks an
	// active ordinary closer to own cleanup just before that closer defers.
	closeRetryRequested bool
	// done is closed when the in-flight statement finishes, so a closer can
	// JOIN it without holding a lock.
	done chan struct{}
	// lastUsed drives the idle timeout.
	lastUsed time.Time

	// The holder's identity, captured AT OPEN because that is the only place it
	// is all in hand at once. The idle-in-transaction heartbeat fires from the
	// janitor sweep, which has a session pointer and nothing else; re-deriving a
	// username there would mean a store round trip inside the sweep loop, per
	// holder, every thirty minutes -- a new failure mode in the one path that
	// must keep running when the store is unhappy.
	//
	// patID is the PAT's ROW ID and never the token. It is what an operator
	// needs in order to revoke, and worth nothing to anyone who reads the audit
	// trail.
	holderUser string
	holderIP   string
	patID      int64
	// acquiredAt is when this session took its backend, which is a different
	// question from when its current transaction opened.
	acquiredAt time.Time

	// stmts counts statements attempted on this session, and lastSQL keeps the
	// most recent one's text for the heartbeat's preview. Guarded by mu.
	stmts   int
	lastSQL string

	// hbTx and hbCursor are the heartbeat's monotonic emission cursor.
	//
	// The cursor exists so a sweep that runs every few seconds emits ONE record
	// per thirty-minute interval rather than one per sweep. It is keyed by the
	// transaction episode, so a new transaction on the same session starts at
	// zero rather than inheriting the previous one's count.
	hbTx     string
	hbCursor int

	// The session's one transaction. All of these
	// fields are guarded by mu.
	tx dao.ContextTxConn
	// pc is the session's PINNED backend connection, set on a
	// postgres WIRE session by the first WireQuery and held for the session's
	// life. Every raw simple-query dispatch runs on it, and the session's
	// transaction is opened THROUGH it (BeginSessionTx), so the raw face and the
	// owned transaction share one backend — a statement inside BEGIN really runs
	// inside it. Token sessions never set it. On close it goes through the
	// release gate (release_gate.go) and nowhere else: the pool gets it back
	// only when a reset has proved it carries none of this client's state
	// forward, and otherwise the physical connection is closed. Handing it
	// back unproved is how one developer's settings become another's.
	pc golibpg.PinnedConn
	// tokenSeq issues recvToken values. Monotonic, so a retired offer can never
	// be confused with a later one.
	tokenSeq uint64
	// gen is this session's generation, stamped at admission and never reused,
	// so a notice posted about one session cannot be honoured against whichever
	// session next occupies its place.
	gen uint64
	// wake knocks on this session's owner so its blocked read returns. It
	// carries nothing: everything the owner needs is published under this
	// session's mutex before the knock, so the knock cannot arrive without
	// what it is about.
	wake func()

	// recvToken is the owner's receive offer: non-zero only between the owner
	// arming its read and that read returning, and a new value each time.
	// Guarded by mu.
	//
	// ONE PIECE OF STATE, IN ONE PLACE, GUARDED BY THE LOCK THAT TAKES THE
	// RESERVATION. It replaced a pair -- a flag here and a mailbox in the front
	// door -- which could disagree: the scheduler could read the flag as open,
	// reserve the session for termination, and only then find the mailbox
	// closed because the read had already returned. The session was then ended
	// with no way to tell its client, having lost a race its client had in fact
	// won. With the offer and the reservation under the same mutex, a
	// reservation is never taken against an offer that has gone.
	recvToken uint64
	// pendingNotice is published under mu in the SAME critical section as the
	// reservation, so the owner cannot be woken about a session that was not
	// reserved, nor reserved without being told.
	pendingNotice *DemandNotice
	// demandFinal is this reclamation's single finalisation claim, set in the
	// same hold that took the reservation and consumed by the one call that
	// finalises it.
	//
	// THE GENERATION IS NOT ENOUGH ON ITS OWN. Identity plus generation stays
	// valid until the session leaves the registry, so two callers presenting
	// the same pair before removal both matched, both were told they owned the
	// teardown, and both went on to tear down -- one ending written twice into
	// the trail, by a return value whose whole job was to say that had not
	// happened. A claim can be consumed; a name cannot.
	demandFinal bool
	// demandHeldObjects records whether the session held prepared statements or
	// portals when it was selected. Kept for the same reason as demandIdle:
	// the record has to say what was true when the choice was made.
	demandHeldObjects bool
	// demandIdle is how long this session had been silent AT THE MOMENT IT WAS
	// SELECTED, kept because that is the fact the decision rested on.
	//
	// RECORDED RATHER THAN RECOMPUTED. The obvious thing is to measure the
	// silence again when the ending is written, and it is wrong: by then the
	// knock, the frame and a bounded flush have all happened, so a slow or
	// unresponsive client inflates the very number offered as justification for
	// ending it. The record has to say what was true when the choice was made.
	demandIdle time.Duration

	// reg is the registry this session was admitted to, or nil for a session
	// that never was. It exists so the transaction counter the admission queue
	// reads can be maintained where transactions actually start and end.
	reg *sessionRegistry

	// ext is the session's extended-protocol namespace (F2, matrix §4a): its
	// wire-level prepared statements and portals. Nil until the first extended
	// frame, and dropped with the session, so the objects cannot outlive the
	// connection whose backend holds them.
	ext              *extObjects
	txPhase          txPhase
	txID             string
	txOpened         time.Time
	txOpenedMayWrite bool
	// targetXID is the target's own transaction id, captured at BEGIN. It is
	// the reconciler's only oracle after a crash, so it is held on the
	// session for the length of the transaction and written onto the
	// commit_started row — the one place it can ever be needed.
	targetXID string
	// limits are resolved once at BEGIN from the engine defaults and the
	// connection's own profile, so a transaction is bounded by what was
	// configured when it opened rather than by whatever config says later.
	limits txLimits
}

// clearTxLocked clears every field owned by the attached transaction. The
// caller must hold s.mu.
func (s *session) clearTxLocked() {
	// matrix §4a: portals do not survive the transaction — named and unnamed alike —
	// while prepared statements do. This is the single point every transaction
	// end passes through (commit, rollback, abort, implicit-block end, failed-tx
	// recovery, authority demotion), so hooking it here is what makes the rule
	// hold no matter WHICH protocol ended the transaction. A client that binds a
	// portal over the extended protocol and then sends a simple COMMIT — lib/pq
	// does exactly this — must not be left naming a portal the backend destroyed.
	if s.ext != nil {
		s.ext.dropAllPortals()
	}
	if s.tx != nil {
		// Safe under s.mu: this takes the leaf lock only. See txMu.
		s.reg.noteTxEnded(s.reservation.LeaseConn, s.id)
	}
	s.tx = nil
	s.txPhase = txNone
	s.txID = ""
	s.txOpened = time.Time{}
	s.txOpenedMayWrite = false
	s.targetXID = ""
	s.limits = txLimits{}
}

func (s *session) get() sessionState { return sessionState(s.state.Load()) }

// sessionRegistry holds the open sessions and enforces the caps.
type sessionRegistry struct {
	mu       sync.Mutex
	byID     map[SessionID]*session
	perUser  map[int64]int
	draining map[int64]bool

	perUserCap int
	globalCap  int

	// Front-door wire leases, per target connection. A wire
	// session holds a PHYSICAL connection for its whole lifetime, so this
	// cap is what stops the front door consuming a pool the interactive
	// surfaces and the engine's own control queries also need.
	//
	// Counted in the SAME registry and under the SAME mutex as the session
	// caps, because matrix row 2.7 requires them acquired as one operation:
	// two locks would reintroduce the check-then-reserve gap between them,
	// and a cap observed free must be the cap acquired.
	leases   map[int64]int
	leaseCap int

	// line is the instance-wide order front-door requests wait in when a cap
	// they could clear by waiting is reached. Guarded by mu, like every cap it
	// schedules against -- see scheduler.go for why that is the whole design.
	line    []*admitWaiter
	lineSeq uint64
	// genSeq stamps each admitted session with a generation that is never
	// reused. See terminalClaim.
	genSeq uint64
	// closed is the reason the line stopped accepting waiters, or nil.
	closed error
	// now is the clock, injectable so a cell can drive a ninety-second wait
	// without sleeping through it.
	now func() time.Time
	// newTimer builds the server wait's timer, injectable so a cell can expire
	// a ninety-second wait without waiting ninety seconds. See serverTimer.
	newTimer func(time.Duration) *time.Timer
	// hookGivingUp fires after a caller has stopped waiting and before it
	// leaves the line -- the window in which a grant can still reach it.
	hookGivingUp func()
	// hookDemandJudged fires while a candidate's lock is held, between the
	// eligibility check and the reservation, so a cell can act in that window.
	hookDemandJudged func()
	// onDemand asks an idle holder on a target to give up its lease, returning
	// whether one was asked. Installed by the engine, which is the only thing
	// that can reach a session's owner.
	onDemand func(leaseConn int64) bool
	// hookWaiterQueued fires once a request is in line, so a cell can act on
	// that fact instead of polling for it.
	hookWaiterQueued func(seq uint64)

	// txMu guards txWithinBound alone and is A LEAF: nothing taken while it is
	// held, ever. That is what makes it safe to update these facts from under
	// a SESSION's mutex, which is where transactions actually begin and end.
	//
	// THE ALTERNATIVE WAS A LOCK INVERSION. Updating them under the registry
	// mutex would mean taking registry-then-session in one place and
	// session-then-registry in another, which is a deadlock the day anything
	// under the registry lock reads a session -- and demand reclamation, which
	// selects a session to terminate, is exactly that.
	txMu sync.Mutex
	// txWithinBound is, per target, when each transaction-holding session's
	// outer bound expires. A DEADLINE RATHER THAN A COUNT, because the
	// question row 5 asks is not "is a transaction open" but "is anything
	// going to be released": a transaction already past its bound is going to
	// be reclaimed, so capacity IS coming and the request must wait for it
	// rather than be told nothing is.
	txWithinBound map[int64]map[SessionID]time.Time

	// demandMu guards demandWanted and demandPromised alone and is A LEAF, for
	// exactly the reason txMu is: a demand reservation is taken under a
	// SESSION's mutex, and recording it under the registry mutex from there
	// would take registry-then-session in one place and session-then-registry
	// in another. That is the inversion this package already refuses once.
	demandMu sync.Mutex
	// demandWanted counts, per target, the queued requests that need a lease
	// reclaimed and have not been answered. ONE PER WAITER, cleared when the
	// waiter leaves the line by any route.
	demandWanted map[int64]int
	// demandPromised is, per target, the sessions already reserved for
	// reclamation whose leases have not yet come back.
	//
	// WITHOUT IT, DEMAND EITHER UNDER- OR OVER-SHOOTS. Asking once per arrival
	// leaves a queued request waiting to expiry when a holder becomes
	// reclaimable a moment later -- nothing releases on its own under
	// lease-per-session, so nothing would ever ask again. Asking on every
	// offer instead would end three sessions for one waiter. The retry is
	// allowed only while more requests are waiting than reclamations are
	// already coming.
	demandPromised map[int64]map[SessionID]struct{}

	// resident is the global weighted memory budget.
	// The session's FIXED OVERHEAD is charged here as the fourth member of
	// the reservation — its absence is what recreates the gap for memory
	// while the other three are protected.
	resident    int64
	residentCap int64

	// Test hooks. They are nil in production and exist because the binding
	// concurrency-testing convention requires a competing transition to be
	// driven INSIDE the window between a guard's last check and its effect —
	// which is unreachable from outside the function. Setting up conflicting
	// state before the call only ever tests the entry condition.
	hookAfterAdmitCheck func()
	hookAfterStateCheck func()
}

func newSessionRegistry(perUser, global int) *sessionRegistry {
	return &sessionRegistry{
		byID:          map[SessionID]*session{},
		perUser:       map[int64]int{},
		draining:      map[int64]bool{},
		perUserCap:    perUser,
		globalCap:     global,
		leases:        map[int64]int{},
		txWithinBound: map[int64]map[SessionID]time.Time{},
	}
}

// newSessionID returns an unguessable identifier.
func newSessionID() (SessionID, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("exec: generating a session id: %w", err)
	}
	return SessionID(base64.RawURLEncoding.EncodeToString(b[:])), nil
}

// admit reserves a slot and inserts the session, or refuses.
//
// The count check and the insert are ONE critical section on purpose. Split
// them and two callers at the cap boundary both see room and both insert —
// the exact defect the convention's window rule exists to catch, which is why
// hookAfterAdmitCheck lets a test drive a competing admit through the gap.
// ErrLeaseCapExceeded reports the per-target wire-lease cap,
// registered in the stable identity list by marked extension).
//
// A distinct identity from the session caps because the operator's remedy
// differs: a session cap says this user or this server is at its limit, while
// this says the TARGET's pool has no lease left — raise pool_max_conns, or
// lower reserved_headroom, or accept fewer concurrent wire sessions on that
// database.
var ErrLeaseCapExceeded = errors.New("exec: lease-cap-exceeded")

// ErrResidentBudgetExceeded reports the global weighted memory budget.
var ErrResidentBudgetExceeded = errors.New("exec: resident-budget-exceeded")

// reservation is what a front-door session acquires as ONE operation.
type reservation struct {
	// LeaseConn is the target connection a wire lease was taken on, 0 for a
	// session that holds none (the interactive surfaces).
	LeaseConn int64
	// Overhead is the fixed memory charge held for this session's lifetime.
	Overhead int64
}

// admitWithLease is admit plus the two front-door members: a per-target wire
// lease and the session's fixed overhead charge.
//
// FOUR members acquired under ONE lock, which is matrix row 2.7's
// requirement and not a convenience. Taking the session slots here and the
// lease somewhere else would put a window between them where a cap observed
// free is not the cap acquired — precisely the defect the PAT cap had, and
// the one the ADR spells out as "partial reservation is impossible".
//
// Everything is released together on failure, so a refused connection holds
// nothing. That is why the rollbacks below are unwound in reverse rather
// than left to a deferred cleanup: a partial hold is worse than a refusal,
// because nothing is coming to release it.
func (r *sessionRegistry) admitWithLease(s *session, leaseConn int64, overhead int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admitLocked(s, leaseConn, overhead)
}

// admitLocked is admitWithLease's body with the lock already held.
//
// SPLIT OUT SO THE QUEUE CAN ADMIT A WAITER INSIDE THE SAME CRITICAL SECTION
// AS THE RELEASE THAT FREED THE CAPACITY. Everything the four-member rule says
// still holds: this is one atomic reservation, and the only change is who is
// holding the lock when it runs.
func (r *sessionRegistry) admitLocked(s *session, leaseConn int64, overhead int64) error {
	if r.draining[s.connID] {
		return fmt.Errorf("%w: connection %d", ErrConnectionDraining, s.connID)
	}
	if len(r.byID) >= r.globalCap {
		return fmt.Errorf("%w: the server is at its limit of %d open sessions",
			ErrSessionCapExceeded, r.globalCap)
	}
	if r.perUser[s.userID] >= r.perUserCap {
		return fmt.Errorf("%w: you already have %d open sessions, the per-user limit",
			ErrSessionCapExceeded, r.perUserCap)
	}
	if leaseConn != 0 && r.leaseCap > 0 && r.leases[leaseConn] >= r.leaseCap {
		return fmt.Errorf("%w: connection %d is at its limit of %d concurrent wire sessions",
			ErrLeaseCapExceeded, leaseConn, r.leaseCap)
	}
	// A NEGATIVE charge is refused rather than accepted as a credit. This is
	// the resource-accounting choke point, and the way an accounting bug
	// becomes a security bug is a caller that hands it a number which makes
	// the budget grow: a negative overhead would raise the remaining
	// allowance for everyone else and the cap would read as satisfied
	// forever. No production caller passes one today; that is a reason to
	// make it impossible now rather than a reason to leave it.
	if overhead < 0 {
		return fmt.Errorf("%w: refusing a negative charge of %d bytes",
			ErrResidentBudgetExceeded, overhead)
	}
	// Compared WITHOUT the addition. `resident + overhead` overflows int64
	// for a large enough overhead and wraps NEGATIVE, which compares below
	// the cap and admits the one reservation that should certainly have been
	// refused. Subtracting from the cap cannot overflow, because both terms
	// are already non-negative and bounded by it.
	if r.residentCap > 0 && overhead > r.residentCap-r.resident {
		return fmt.Errorf("%w: %d bytes held of %d, and this session asks for %d",
			ErrResidentBudgetExceeded, r.resident, r.residentCap, overhead)
	}

	if h := r.hookAfterAdmitCheck; h != nil {
		// Inside the window: every cap has been checked and nothing has been
		// taken. The lock is still held, which is the property under test.
		h()
	}

	r.byID[s.id] = s
	r.perUser[s.userID]++
	if leaseConn != 0 {
		r.leases[leaseConn]++
	}
	r.resident += overhead
	s.reservation = reservation{LeaseConn: leaseConn, Overhead: overhead}
	// The generation is stamped at admission and never reused, so a terminal
	// claim taken against this session cannot be honoured against whichever
	// session next occupies its place.
	r.genSeq++
	s.gen = r.genSeq
	// The session learns its registry here so that the transaction counter
	// this schedules against can be kept at the two places a transaction
	// actually begins and ends, rather than re-derived under the wrong lock.
	s.reg = r
	return nil
}

// noteTxOpened and noteTxEnded record when a lease-holding transaction's outer
// bound expires. Safe to call with the session's mutex held: txMu is a leaf.
func (r *sessionRegistry) noteTxOpened(leaseConn int64, id SessionID, boundUntil time.Time) {
	if r == nil || leaseConn == 0 {
		return
	}
	r.txMu.Lock()
	if r.txWithinBound[leaseConn] == nil {
		r.txWithinBound[leaseConn] = map[SessionID]time.Time{}
	}
	r.txWithinBound[leaseConn][id] = boundUntil
	r.txMu.Unlock()
}

func (r *sessionRegistry) noteTxEnded(leaseConn int64, id SessionID) {
	if r == nil || leaseConn == 0 {
		return
	}
	r.txMu.Lock()
	if m := r.txWithinBound[leaseConn]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			// Deleted rather than left empty: a map of every target that ever
			// held a transaction grows without bound on a long-lived daemon.
			delete(r.txWithinBound, leaseConn)
		}
	}
	r.txMu.Unlock()
}

// txWithinBoundCount reports how many of a target's transactions are still
// inside their bounds at this instant.
func (r *sessionRegistry) txWithinBoundCount(leaseConn int64, now time.Time) int {
	r.txMu.Lock()
	defer r.txMu.Unlock()
	n := 0
	for _, until := range r.txWithinBound[leaseConn] {
		// STRICTLY BEFORE, so the instant a bound expires the transaction stops
		// counting. At equality the expiry rung owns it and capacity is coming.
		if now.Before(until) {
			n++
		}
	}
	return n
}

// allLeasesInTransactionLocked reports whether every lease on this target is
// held by a transaction that is still inside its bounds. Caller holds r.mu.
//
// READ FROM THE SAME SNAPSHOT AS THE CAP THAT JUST REFUSED, which is why it is
// a counter and why this runs under the lock that made the refusal. Two
// separately-taken readings could disagree, and the refusal identity they
// decide -- "nothing is coming, do not wait" -- is exactly the one that must
// not be issued on a stale view.
func (r *sessionRegistry) allLeasesInTransactionLocked(leaseConn int64) bool {
	if leaseConn == 0 || r.leaseCap <= 0 {
		return false
	}
	held := r.leases[leaseConn]
	if held < r.leaseCap {
		return false
	}
	// EVERY lease must be held by a transaction that is STILL INSIDE ITS
	// BOUND. One that is past its bound is going to be reclaimed, so capacity
	// is coming and this request must be allowed to wait for it -- telling it
	// "nothing is coming" would be false, and it would be told so immediately
	// rather than being served moments later.
	return r.txWithinBoundCount(leaseConn, r.clock()) >= held
}

// releaseReservation gives back everything admitWithLease took. Called from
// remove, so a session cannot leave the registry while still holding a lease.
func (r *sessionRegistry) releaseReservation(s *session) {
	if s.reservation.LeaseConn != 0 {
		// THE PROMISE IS DISCHARGED WHERE THE LEASE COMES BACK, not where the
		// reclamation was decided. Between those two points the lease is still
		// held, and a retry that counted it as returned would reclaim a second
		// session for a request the first one already answers.
		r.dischargeDemand(s.reservation.LeaseConn, s.id)
		if n := r.leases[s.reservation.LeaseConn]; n > 1 {
			r.leases[s.reservation.LeaseConn] = n - 1
		} else {
			// Delete rather than leave a zero: a map of connections that
			// once had leases grows without bound on a long-lived daemon.
			delete(r.leases, s.reservation.LeaseConn)
		}
	}
	r.resident -= s.reservation.Overhead
	if r.resident < 0 {
		// Defensive: a negative budget means a double release, which would
		// silently hand out capacity that does not exist.
		r.resident = 0
	}
	s.reservation = reservation{}
}

// leaseCount reports the wire leases held on a connection. Test-support.
// inTransactionHoldingBackend counts the sessions that hold a physical backend
// AND have a transaction open on it.
//
// THIS IS THE NUMBER THAT DECIDES WHETHER WAITING IS POINTLESS. A slot held by
// an idle session can be reclaimed the moment somebody needs it; a slot held
// by a live transaction cannot, because reclaiming it would roll back work the
// caller has not finished and did not ask us to abandon. When every slot is
// the second kind there is nothing for a newcomer to wait FOR, which is the
// whole justification for refusing before the queue rather than after ninety
// seconds in it.
//
// BOTH CONDITIONS, NOT EITHER. A session with an open transaction but no
// pinned backend is holding no capacity, and counting it would refuse
// newcomers on behalf of a slot that does not exist.
func (r *sessionRegistry) inTransactionHoldingBackend() int {
	r.mu.Lock()
	sessions := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	// Counted OUTSIDE the registry lock, because reading a session's own state
	// takes that session's mutex and holding both at once is how this package
	// would acquire a lock-ordering problem it does not have today.
	n := 0
	for _, s := range sessions {
		s.mu.Lock()
		if s.tx != nil && s.pc != nil {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

func (r *sessionRegistry) leaseCount(connID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases[connID]
}

// residentHeld reports the charged memory. Test-support.
func (r *sessionRegistry) residentHeld() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resident
}

// admit reserves the session slots only, for surfaces that hold no wire
// lease — the TUI, Lua and Web clients, which use pooled connections.
//
// It delegates rather than duplicating, so there is exactly ONE reservation
// path and one place where the ordering of the checks lives. Two
// implementations of "is there room" is how the interactive surfaces and the
// front door come to disagree about a cap they share.
func (r *sessionRegistry) admit(s *session) error {
	return r.admitWithLease(s, 0, 0)
}

// lookup returns a session owned by userID, or ErrSessionNotFound.
//
// Ownership is checked here rather than by the caller so there is one place
// that can get it wrong, and the not-found and not-yours answers are built
// from the same return so they cannot drift apart.
func (r *sessionRegistry) lookup(id SessionID, userID int64) (*session, error) {
	r.mu.Lock()
	s, ok := r.byID[id]
	r.mu.Unlock()
	if !ok || s.userID != userID || s.get() != sessOpen {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// remove drops a session from the registry. Idempotent.
func (r *sessionRegistry) remove(s *session) {
	r.mu.Lock()
	if _, ok := r.byID[s.id]; ok {
		delete(r.byID, s.id)
		if n := r.perUser[s.userID] - 1; n > 0 {
			r.perUser[s.userID] = n
		} else {
			delete(r.perUser, s.userID)
		}
		// The lease and the memory charge go with the session, under the
		// SAME lock that removed it. Releasing them separately would let a
		// session leave the registry while still counted against its
		// target's lease cap — a leak that only shows up as a target
		// mysteriously refusing connections it has capacity for.
		r.releaseReservation(s)
		// THE FREED CAPACITY GOES STRAIGHT TO THE LONGEST-WAITING REQUEST THAT
		// CAN USE IT, INSIDE THIS SAME LOCK. Unlocking first and letting
		// waiters race for it is the gap that makes a queue decorative: the
		// slot would be exposed to every newcomer, and the request that waited
		// longest is the least likely to be running at that instant.
		r.serveLine()
	}
	r.mu.Unlock()
}

// setDraining marks a connection as shutting down and returns its sessions.
//
// Marking comes FIRST and under the same lock as the snapshot, so no session
// can be admitted onto a connection that is already being torn down — the
// ordering the design requires of conn.delete.
func (r *sessionRegistry) setDraining(connID int64) []*session {
	r.mu.Lock()
	r.draining[connID] = true
	var out []*session
	for _, s := range r.byID {
		if s.connID == connID {
			out = append(out, s)
		}
	}
	r.mu.Unlock()
	return out
}

// clearDraining lets a connection be used again (a failed delete).
func (r *sessionRegistry) clearDraining(connID int64) {
	r.mu.Lock()
	delete(r.draining, connID)
	r.mu.Unlock()
}

// wireSessions returns the front-door sessions on connID. Internal sessions
// have no wire lease and are deliberately excluded.
func (r *sessionRegistry) wireSessions(connID int64) []*session {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*session
	for _, s := range r.byID {
		if s.connID == connID && s.reservation.LeaseConn != 0 {
			out = append(out, s)
		}
	}
	return out
}

// isDraining reports whether a connection is shutting down.
func (r *sessionRegistry) isDraining(connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining[connID]
}

// snapshot returns every open session.
// holderCount reports how many sessions this account currently holds.
//
// The heartbeat carries it because one holder idling is a person thinking, and
// the same person holding nine backends while idling is a leak -- and the
// record that shows only the one backend cannot tell those apart.
func (r *sessionRegistry) holderCount(userID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perUser[userID]
}

func (r *sessionRegistry) snapshot() []*session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s)
	}
	return out
}

// begin claims the session's single execution slot.
func (s *session) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return ErrSessionBusy
	}
	s.busy = true
	s.done = make(chan struct{})
	return nil
}

// finish releases the execution slot and wakes anyone joining it.
func (s *session) finish() {
	s.mu.Lock()
	s.busy = false
	s.lastUsed = time.Now()
	done := s.done
	s.done = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// noteStatement records that a statement was attempted on this session.
//
// Called from the one place every attempt passes through, so the count cannot
// drift between the simple, extended and token paths -- three separate hooks
// is how one of them ends up not counting, and a holder that reports zero
// statements looks like an idle connection rather than a long transaction.
//
// It keeps the statement TEXT, which is the same text already written to the
// exec audit row and the history script column. Bind values are not part of
// it: the extended protocol carries them separately and they are never
// interpolated into what is stored here.
func (s *session) noteStatement(sql string) {
	s.mu.Lock()
	s.stmts++
	s.lastSQL = sql
	s.mu.Unlock()
}

// idleFor reports how long the session has been idle.
func (s *session) idleFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return 0
	}
	return now.Sub(s.lastUsed)
}

// beginClose moves open → closing and reports whether THIS caller owns the
// terminal transition.
//
// CAS, not a mutex: several closers can arrive at once — an explicit
// CloseSession, the idle reaper, a connection being deleted, engine shutdown
// — and the teardown must happen exactly once. The winner is whoever moves
// the state; everyone else gets false and does nothing.
// There is deliberately NO test hook in here. The other guards in this file
// take one because they have a real window — a check, then a separate effect,
// with a gap a competing caller can be driven into. This one does not: the
// CAS is the check and the effect in a single instruction, so there is no gap
// to reach into, and a hook could only sit outside the operation where it
// proves nothing. See the note on TestSession_ManyClosersOneOwner.
func (s *session) beginClose(ip, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beginCloseLocked(ip, reason)
}

// beginCloseLocked is beginClose with the session's mutex already held.
//
// SPLIT OUT SO A CALLER CAN DECIDE AND RESERVE WITHOUT LETTING GO. Demand
// reclamation has to check that a session is idle, quiet and reachable and then
// claim it, and if it released the lock between those two steps the session
// could start a statement in the gap -- so it would be terminated after
// becoming active, which is the one thing the predicate exists to prevent.
func (s *session) beginCloseLocked(ip, reason string) bool {
	if !s.state.CompareAndSwap(int32(sessOpen), int32(sessClosing)) {
		return false
	}
	s.closeIP, s.closeWhy = ip, reason
	s.closeActive = true
	return true
}

// runContext is the context ONE statement runs on: the session's lifetime and
// the caller's cancellation, together.
//
// It used to be the session's context alone, and the comment there defended
// that choice — a client hanging up mid-statement should not abandon work on
// a live production database. That reasoning was wrong in an important way.
// Stateless Execute already stops when its handler is cancelled; a session
// statement did not, so `SELECT pg_sleep(3)` kept running on the target long
// after the client that asked for it had gone. The inconsistency is the bug:
// two paths through the same engine answered the same question differently,
// and the one that ignored cancellation was the one holding a pinned
// transaction on a production database.
//
// Both bounds are needed and neither subsumes the other. The session context
// is the OUTER bound — a closed or timed-out session must stop work even if
// the caller is happily waiting — and the caller's is the INNER one. A
// cancelled statement does not end the session or its transaction: the
// statement returns an error, the state machine records the outcome, and the
// next statement or the rollback runs on a fresh context of its own.
func (s *session) runContext(caller context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(caller, cancel)
	// The handle is kept so the ENGINE can stop this statement without
	// killing the session: a timeout ends the transaction, not the session,
	// and the session's own cancel is too blunt an instrument for that.
	s.mu.Lock()
	s.runCancel = cancel
	s.mu.Unlock()
	return ctx, func() {
		stop()
		cancel()
		s.mu.Lock()
		s.runCancel = nil
		s.mu.Unlock()
	}
}

// cancelInFlight stops the statement currently running, if any. It does not
// end the session.
func (s *session) cancelInFlight() {
	s.mu.Lock()
	cancel := s.runCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// claimTeardown takes the one-statement slot on behalf of a teardown.
//
// It closes the window between joining the in-flight statement and detaching
// the transaction. Without it, a statement arriving in that window starts on
// a transaction that is about to be rolled back out from under it — the
// engine having just proven the session idle, and then acted on a fact that
// was no longer true.
//
// It reports false if a statement got there first; the caller must then not
// proceed, exactly as for a failed join.
func (s *session) claimTeardown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.tearingDown = true
	s.done = make(chan struct{})
	return true
}

// releaseTeardown gives the slot back.
func (s *session) releaseTeardown() {
	s.mu.Lock()
	s.busy = false
	s.tearingDown = false
	done := s.done
	s.done = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// joinInFlight waits for the in-flight statement to finish and REPORTS
// whether it actually did.
//
// The reporting is the point. The wait this replaced returned either way, so
// every caller proceeded to roll back whether or not the statement had
// stopped — issuing RollbackContext on a connection that was still
// executing. A join whose result is discarded is not a join; it is a pause.
func (s *session) joinInFlight(ctx context.Context) error {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// inTransaction reports whether a transaction is currently open.
//
// This is the session's own state, which is the point: a caller that needs to
// know had better ask rather than keep a second copy that can disagree.
func (s *session) inTransaction() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txPhase != txNone
}

// transferClose publishes an overriding close reason and claims finalizer
// ownership when no owner is active. It covers both an open session and a
// closing session whose earlier owner explicitly deferred for retry.
func (s *session) transferClose(ip, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.get() {
	case sessOpen:
		if !s.state.CompareAndSwap(int32(sessOpen), int32(sessClosing)) {
			return false
		}
		s.closeIP, s.closeWhy = ip, reason
		s.closeActive = true
		return true
	case sessClosing:
		s.closeIP, s.closeWhy = ip, reason
		if !s.closeActive {
			s.closeActive = true
			return true
		}
		s.closeRetryRequested = true
	}
	return false
}

// claimCloseRetry takes finalizer ownership only after an earlier owner
// explicitly deferred. A closing state alone is not enough: the original
// owner may still be quiescing or rolling back.
func (s *session) claimCloseRetry() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get() != sessClosing || s.closeActive {
		return false
	}
	s.closeActive = true
	return true
}

// releaseCloseForRetry reports whether a transfer arrived while this owner was
// active. In that case ownership remains active and this owner must retry
// immediately; otherwise the next janitor may claim it.
func (s *session) releaseCloseForRetry() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeRetryRequested {
		s.closeRetryRequested = false
		return true
	}
	s.closeActive = false
	return false
}

// finishClose moves closing → closed.
func (s *session) finishClose() { s.state.Store(int32(sessClosed)) }

// auditTag is the session's stamp for exec/exec_result audit lines: the wire
// session id and the client's application_name (matrix claim
// 3.1:application_name#session-audit). Token sessions carry no stamp.
func (s *session) auditTag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wire {
		return ""
	}
	return fmt.Sprintf("session %s app %q", s.id, s.appName)
}
