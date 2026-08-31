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
	// authSessID names the AUTH session row this exec session's authority
	// rests on. A pinned transaction outlives the call that opened it, so the
	// authority has to be re-checkable later without a token — and revocation
	// and expiry both live on that row.
	authSessID int64

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
	// closeIP and closeWhy are kept so a retried close audits the reason the
	// close actually had, rather than inventing one at retry time.
	closeIP  string
	closeWhy string
	// done is closed when the in-flight statement finishes, so a closer can
	// JOIN it without holding a lock.
	done chan struct{}
	// lastUsed drives the idle timeout.
	lastUsed time.Time

	// The session's one transaction (ADR-0074 Amendment 2). All of these
	// fields are guarded by mu.
	tx       dao.ContextTxConn
	txPhase  txPhase
	txID     string
	txOpened time.Time
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

func (s *session) get() sessionState { return sessionState(s.state.Load()) }

// sessionRegistry holds the open sessions and enforces the caps.
type sessionRegistry struct {
	mu       sync.Mutex
	byID     map[SessionID]*session
	perUser  map[int64]int
	draining map[int64]bool

	perUserCap int
	globalCap  int

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
func (r *sessionRegistry) admit(s *session) error {
	r.mu.Lock()
	if r.draining[s.connID] {
		r.mu.Unlock()
		return fmt.Errorf("%w: connection %d", ErrConnectionDraining, s.connID)
	}
	if len(r.byID) >= r.globalCap {
		r.mu.Unlock()
		return fmt.Errorf("%w: the server is at its limit of %d open sessions", ErrSessionCapExceeded, r.globalCap)
	}
	if r.perUser[s.userID] >= r.perUserCap {
		r.mu.Unlock()
		return fmt.Errorf("%w: you already have %d open sessions, the per-user limit", ErrSessionCapExceeded, r.perUserCap)
	}
	if h := r.hookAfterAdmitCheck; h != nil {
		// Inside the window: the counts have been checked and nothing has
		// been inserted yet. The lock is still held, which is precisely the
		// property under test — a hook that deadlocks here is reporting a
		// real defect.
		h()
	}
	r.byID[s.id] = s
	r.perUser[s.userID]++
	r.mu.Unlock()
	return nil
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
func (s *session) beginClose() bool {
	return s.state.CompareAndSwap(int32(sessOpen), int32(sessClosing))
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

// setCloseReason records why a close began, for a retry to audit later.
func (s *session) setCloseReason(ip, reason string) {
	s.mu.Lock()
	s.closeIP, s.closeWhy = ip, reason
	s.mu.Unlock()
}

func (s *session) closeReason() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeIP, s.closeWhy
}

// finishClose moves closing → closed.
func (s *session) finishClose() { s.state.Store(int32(sessClosed)) }
