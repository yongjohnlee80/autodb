package exec

import (
	"errors"
	"sync"
)

// The idle-only shutdown: a restart taken only when nothing it would interrupt
// exists, decided in ONE step.
//
// "Idle" is three facts at one instant: no session holds an open transaction,
// no statement is executing, and no front-door (PostgreSQL-wire) client is
// connected at all — an idle wire client can still hold a prepared statement,
// a suspended portal or session settings a restart would destroy, and no
// automatic restart may disrupt an external client. Neovim's own RPC sessions
// are not counted: they reconnect by their own epoch handling.
//
// ONE DECISION, as sys.shutdown's transaction count is. Counting first and
// closing afterwards would leave a window a statement, a BEGIN or a client
// could start in, and the drain would then destroy it. So each of the three
// has a gate, the decision holds all three, counts under them, and closes all
// three before letting go — "nothing is running" and "nothing can start" are
// the same instant. When anything is counted, nothing is closed.
//
// Lock order: the registry's transaction gate, then the statement gate, then
// the counters. BeginIdleShutdown and AbortIdleShutdown take them in that
// order; a statement or a wire client takes only the last two, in order.

// ErrStatementAdmissionClosed refuses a statement because an idle shutdown has
// closed admission: the daemon is stopping, and a new daemon will serve.
var ErrStatementAdmissionClosed = errors.New("exec: the server is stopping; no new statement may start")

// idleGates are the statement and wire gates, and their counters.
type idleGates struct {
	stmt       sync.RWMutex // held for writing by the decision; for reading while a statement enters
	stmtClosed bool         // guarded by stmt

	mu         sync.Mutex // guards the counters and the wire flag
	executing  int        // statements between their entry and their return
	wireConns  int        // front-door connections admitted and not yet released
	wireClosed bool
	owner      uint64 // the decision that closed all three; 0 = none
}

// enterStatement admits one statement, returning the release it must call
// when the statement has returned — its terminal is recorded by then.
func (e *Engine) enterStatement() (func(), error) {
	g := &e.idle
	g.stmt.RLock()
	defer g.stmt.RUnlock()
	if g.stmtClosed {
		return nil, ErrStatementAdmissionClosed
	}
	g.mu.Lock()
	g.executing++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.executing--
			g.mu.Unlock()
		})
	}, nil
}

// AdmitWireConnection admits one front-door connection, from its accept until
// its close. It answers false while an idle shutdown has closed wire
// admission, and the connection must then be refused.
func (e *Engine) AdmitWireConnection() (release func(), ok bool) {
	g := &e.idle
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wireClosed {
		return nil, false
	}
	g.wireConns++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.wireConns--
			g.mu.Unlock()
		})
	}, true
}

// IdleCounts are what an idle shutdown found running.
type IdleCounts struct {
	InTransaction, Executing, WireSessions int
}

// Busy reports whether anything was running.
func (c IdleCounts) Busy() bool { return c.InTransaction+c.Executing+c.WireSessions > 0 }

// BeginIdleShutdown decides an idle shutdown. With nothing running it closes
// transaction, statement and wire admission and returns the owner token the
// caller must either carry through to the shutdown or hand to
// AbortIdleShutdown. With anything running it closes nothing and returns the
// counts and a zero token. A zero token with no counts means another decision
// already owns admission.
func (e *Engine) BeginIdleShutdown() (IdleCounts, uint64) {
	r, g := e.sessions, &e.idle
	r.txGate.Lock()
	defer r.txGate.Unlock()
	if r.txClosed {
		return IdleCounts{}, 0 // another decision owns admission
	}
	// Under the transaction gate no transaction can open, so this count holds
	// for as long as the gate does.
	counts := IdleCounts{InTransaction: r.countInTransactionLocked()}
	g.stmt.Lock()
	defer g.stmt.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	counts.Executing, counts.WireSessions = g.executing, g.wireConns
	if counts.Busy() {
		return counts, 0
	}
	r.txClosed = true
	r.txSeq++
	r.txOwner = r.txSeq
	g.stmtClosed, g.wireClosed, g.owner = true, true, r.txOwner
	return counts, r.txOwner
}

// AbortIdleShutdown reopens all three gates for the decision that closed them,
// and for nothing else — a stale or absent token is a no-op. It reports
// whether it reopened them.
//
// NOT AbortShutdown: that reopens the transaction gate alone, and a daemon
// left with its statement and wire gates closed would refuse every statement
// and every client for good.
func (e *Engine) AbortIdleShutdown(owner uint64) bool {
	r, g := e.sessions, &e.idle
	r.txGate.Lock()
	defer r.txGate.Unlock()
	g.stmt.Lock()
	defer g.stmt.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if owner == 0 || owner != g.owner || owner != r.txOwner || !r.txClosed {
		return false
	}
	r.txClosed, r.txOwner = false, 0
	g.stmtClosed, g.wireClosed, g.owner = false, false, 0
	return true
}
