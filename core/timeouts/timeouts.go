// Package timeouts holds every timeout in core/exec, core/config and frontdoor.
//
// It is the only place those packages may write a time.<unit> value: the
// guard in timeouts_guard_test.go fails on any other, unless its line says
// "// not a timeout: <kind>" (a ticker, a cache lifetime, a unit conversion).
// The packages alias these constants or read them directly, so a timeout has
// one value and one place to change it.
//
// Three kinds:
//   - policy: changed live by a policy reload, and never above MaxBound;
//   - config: read from the config file at start, and never above MaxBound;
//   - fixed: compiled in.
package timeouts

import "time"

// MaxBound is the longest a policy or config timeout may be. A write above it
// is refused; a stored or configured value above it is clamped on load.
//
// Owner ruling (2026-11-07): no transaction should run longer than two hours.
// Fixed timeouts are held to it by a unit test; IdleHoldingState equals it.
const MaxBound = 2 * time.Hour

// Front door, before authentication (fixed). A client that can't finish a
// phase in time is disconnected. Each phase has its own budget, so a slow TLS
// handshake can't take the time owed to the credential exchange.
const (
	// TLSHandshake bounds the TLS handshake.
	TLSHandshake = 10 * time.Second
	// StartupPacket bounds receiving the startup packet.
	StartupPacket = 10 * time.Second
	// CredentialExchange bounds the credential exchange, inside an auth
	// worker slot. A front-door session's wait in its pool's line and the
	// pin of its backend run inside it too.
	CredentialExchange = 10 * time.Second
)

// Waiting for capacity (fixed).
const (
	// QueueWait is how long a new session waits in its pool's line, and how
	// long a dial whose caller has no deadline waits in its share's line. A
	// front-door wait ends sooner, PinAllowance + HandshakeReserve before
	// CredentialExchange runs out.
	//
	// Not derived from the transaction bounds: how long a transaction may sit
	// idle is not how long a person or a service should wait for an answer.
	QueueWait = 90 * time.Second
	// HandshakeReserve is kept back from the credential deadline, so a
	// front-door session admitted or refused at the last moment can still be
	// told.
	HandshakeReserve = 2 * time.Second
	// PinAllowance is what the line leaves the pin: the dial, TLS, startup,
	// the encoding check and the startup parameters, before HandshakeReserve.
	PinAllowance = 2 * time.Second
	// MemoryLaneWait bounds a front-door request's wait for memory-lane
	// bytes, before it stops holding a statement open.
	MemoryLaneWait = 30 * time.Second
	// ReclaimGrace is how long a session must have been silent before a
	// waiting login may end it to take its place. A pooled client sends its
	// first statement a moment after the session opens, and a request's
	// statements a moment apart; ending a session inside that gap hands the
	// client a dead connection, and the burst that caused the wait turns into
	// failed requests.
	//
	// SHORTER THAN A FRONT-DOOR LOGIN'S WAIT, which is CredentialExchange less
	// PinAllowance and HandshakeReserve (6 s). A login that arrives just as
	// every holder went quiet can still be served by the one that stays quiet
	// longest; a guard test holds the two apart.
	ReclaimGrace = 3 * time.Second
)

// Idle sessions.
const (
	// IdleBetweenMessages (fixed) is how long a front-door connection may be
	// idle between messages, in a transaction or not. Every front-door session
	// holds a backend, so this frees pool capacity.
	IdleBetweenMessages = 30 * time.Minute
	// SessionIdle (policy) closes a session with no transaction and no
	// statement for this long. It reaps the sessions a crashed client left.
	SessionIdle = 10 * time.Minute
)

// Open transactions (policy). An abandoned transaction holds locks and stops
// VACUUM on a production database until something ends it. These end it with
// an audited rollback. The server's own idle_in_transaction_session_timeout is
// set ServerBeltMargin later, so autodb always fires first and can audit what
// it did.
const (
	// IdleInTransaction ends a transaction idle this long. A forgotten BEGIN
	// holds a pool slot that demand reclaim can never take.
	IdleInTransaction = 30 * time.Minute
	// MaxTransaction bounds the total length of one transaction.
	MaxTransaction = 2 * time.Hour
	// MaxTransactionCeiling is the most a per-connection override of
	// MaxTransaction may ask for.
	MaxTransactionCeiling = MaxBound
	// ServerBeltMargin (fixed) is how far behind the engine's deadline the
	// server-side belt is set. It covers the janitor's tick, not a share of
	// the bound, so it does not scale with it.
	ServerBeltMargin = 30 * time.Second
)

// Session-owned state (fixed).
const (
	// IdleHoldingState ends a session idle while holding named statements or
	// portals, measured from the last progress on what it holds. A client may
	// hold a prepared statement across a quiet period; one that has not
	// touched it in two hours has abandoned it. Demand reclaim may end such a
	// session sooner, and only under pressure.
	IdleHoldingState = 2 * time.Hour
)

// Physical backends.
const (
	// BackendIdle (config) closes a pooled socket unused this long. It applies
	// only to targets outside every connection pool: a pool's backends are
	// never reused.
	BackendIdle = 10 * time.Minute
	// StrayBackendIdle (fixed): a pool backend is idle only when its caller
	// left while it was dialled. Its pgxpool reaps it after this, checking
	// this often.
	StrayBackendIdle = 1 * time.Second
	// BackendLifetime (config) retires a pooled socket after this long, so a
	// rotated credential or a restarted primary takes effect without a
	// restart.
	BackendLifetime = 60 * time.Minute
	// BackendReset (fixed) bounds resetting a backend for reuse; on timeout
	// it is destroyed. Only targets outside every pool reuse backends.
	BackendReset = 10 * time.Second
	// BackendConnect (fixed) bounds connecting a new pgx backend, unless the
	// DSN sets connect_timeout.
	BackendConnect = 2 * time.Minute
	// CancelDialFallback (fixed) is the net.Conn deadline fallback on a
	// cancel request's dial.
	CancelDialFallback = 5 * time.Second
)

// Front-door streams (fixed). Each is its own constant though several share a
// value: they measure different things, and a change to one must not change
// the others.
const (
	// OutputStall bounds one write to a client that won't read what it asked
	// for.
	OutputStall = 30 * time.Second
	// FrameStall bounds a client that started a message and stopped
	// mid-frame.
	FrameStall = 30 * time.Second
	// SegmentStall bounds an extended-protocol segment that stops arriving
	// before Sync.
	SegmentStall = 30 * time.Second
	// GoodbyeWrite bounds writing the frame that tells a client why it is
	// being disconnected.
	GoodbyeWrite = 2 * time.Second
)

// Shutdown and cleanup (fixed). Each is generous, because losing a record or a
// clean rollback is a real loss, and bounded, because a teardown that never
// finishes is a worse one.
const (
	// Record bounds recording an outcome.
	Record = 10 * time.Second
	// AuditWrite bounds writing a cleanup audit row.
	AuditWrite = 10 * time.Second
	// TxCleanup bounds a commit or rollback taken on a fresh context.
	TxCleanup = 30 * time.Second
	// EngineClose bounds one target pool's close during engine shutdown.
	EngineClose = 15 * time.Second
	// CloseQuiesce bounds a closing session's wait for its statement to stop.
	// Longer than Quiesce: a close is usually a person asking, and finishing
	// the rollback properly is worth a few more seconds.
	CloseQuiesce = 15 * time.Second
	// PoolClose bounds how long deleting a connection waits for its target
	// pool to shut down.
	PoolClose = 20 * time.Second
	// Quiesce bounds a teardown's wait for a cancelled statement to stop, at
	// a transaction deadline.
	Quiesce = 10 * time.Second
)

// Remote servers (fixed).
const (
	// RemoteHandshake bounds the handshake with a remote autodb server.
	RemoteHandshake = 10 * time.Second
	// RemoteReconnectGrace is how long a dropped remote session may
	// reconnect.
	RemoteReconnectGrace = 2 * time.Minute
)
