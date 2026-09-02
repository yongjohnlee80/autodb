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

// ExecSession — an engine-owned client session (ADR-0074 §1).
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
	// running one. Sessions serialize; they do not queue (ADR-0074 §1), so a
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
	// it (ADR-0074 §1 lifecycle linearization).
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

	// The session's one transaction (ADR-0074 Amendment 2). All of these
	// fields are guarded by mu.
	tx dao.ContextTxConn
	// pc is the session's PINNED backend connection (golib ADR-0018), set on a
	// postgres WIRE session by the first WireQuery and held for the session's
	// life. Every raw simple-query dispatch runs on it, and the session's
	// transaction is opened THROUGH it (BeginSessionTx), so the raw face and the
	// owned transaction share one backend — a statement inside BEGIN really runs
	// inside it. Token sessions never set it. Discarded, never released, on
	// close: the wire is the client's for the connection's life and a pooled
	// recycle of it would carry session state to another user.
	pc               golibpg.PinnedConn
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

	// Front-door wire leases, per target connection (ADR-0075 §3). A wire
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

	// resident is the global weighted memory budget (ADR-0075 §4 rev 5).
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
		byID:       map[SessionID]*session{},
		perUser:    map[int64]int{},
		draining:   map[int64]bool{},
		perUserCap: perUser,
		globalCap:  global,
		leases:     map[int64]int{},
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
// ErrLeaseCapExceeded reports the per-target wire-lease cap (ADR-0075 §3,
// registered in ADR-0074 §8a's stable identity list by marked extension).
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
	return nil
}

// releaseReservation gives back everything admitWithLease took. Called from
// remove, so a session cannot leave the registry while still holding a lease.
func (r *sessionRegistry) releaseReservation(s *session) {
	if s.reservation.LeaseConn != 0 {
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
	}
	r.mu.Unlock()
}

// setDraining marks a connection as shutting down and returns its sessions.
//
// Marking comes FIRST and under the same lock as the snapshot, so no session
// can be admitted onto a connection that is already being torn down — the
// ordering ADR-0074 §1 requires of conn.delete.
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

// isDraining reports whether a connection is shutting down.
func (r *sessionRegistry) isDraining(connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining[connID]
}

// snapshot returns every open session.
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
