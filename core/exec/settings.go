package exec

import (
	"fmt"
	"time"
)

// Settings is the engine's EFFECTIVE configuration — what is actually in
// force, after defaults and options, rather than what a file says.
//
// It is deliberately real API rather than a test hook. Every value here was
// once parsed, validated, defaulted and then dropped by the construction
// site, and nothing failed: an operator who set max_tx_duration got the
// built-in default with no way to discover it. A readout of what took effect
// is the thing that makes that class of bug visible instead of silent, and it
// is what an operator asking "is my configuration live?" needs.
type Settings struct {
	MaxStatementBytes    int
	MaxSessionsPerUser   int
	MaxSessionsGlobal    int
	SessionIdleTimeout   time.Duration
	IdleInTxTimeout      time.Duration
	MaxTxDuration        time.Duration
	DebugIdleInTxTimeout time.Duration
	MaxTxDurationCeiling time.Duration
	PoolMaxConns         int
	PoolMaxConnIdleTime  time.Duration
	PoolMaxConnLifetime  time.Duration

	// LeaseCap bounds concurrent wire sessions per target pool, and
	// ResidentBudget the memory they may reserve in total. Both were set
	// only by tests until the daemon wiring landed: zero here means the
	// guard is off.
	LeaseCap       int
	ResidentBudget int64

	// PolicyGeneration counts published reloads. Zero is the configuration the
	// daemon started with.
	PolicyGeneration uint64

	// TargetConns is the aggregate production-connection budget and its live
	// state. Zero Configured means no budget is in force, which is what an
	// engine embedded without a front door gets.
	TargetConns LedgerSnapshot
}

// Settings returns the effective configuration.
func (e *Engine) Settings() Settings {
	// BOTH HALVES READ UNDER ONE LOCK.
	//
	// Loading the policy pointer here and taking the ledger snapshot
	// afterwards is a torn read, and locking the WRITER did not prevent it: a
	// reader could load the old pointer, be descheduled while a reload
	// published, and come back to pair those old timeouts with the new budget.
	// That tuple never existed at any instant, and the person it is handed to
	// is the operator who just made the change and is looking for it.
	p, conns := e.coherentPolicyAndLedger()
	return Settings{
		MaxStatementBytes:    e.maxStatementBytes,
		MaxSessionsPerUser:   e.sessions.perUserCap,
		MaxSessionsGlobal:    e.sessions.globalCap,
		SessionIdleTimeout:   p.sessionIdle,
		IdleInTxTimeout:      p.tx.idleInTx,
		MaxTxDuration:        p.tx.maxTx,
		DebugIdleInTxTimeout: p.debugIdle,
		MaxTxDurationCeiling: p.maxTxCeiling,
		PoolMaxConns:         e.poolMaxConns,
		PoolMaxConnIdleTime:  e.poolMaxConnIdleTime,
		PoolMaxConnLifetime:  e.poolMaxConnLifetime,
		LeaseCap:             e.sessions.leaseCap,
		ResidentBudget:       e.sessions.residentCap,
		PolicyGeneration:     p.generation,
		TargetConns:          conns,
	}
}

// coherentPolicyAndLedger reads the live policy and the ledger as ONE reading.
//
// With a ledger, the policy pointer is loaded INSIDE the ledger's lock, which
// is the same lock a publication holds while it swaps that pointer. A reader
// therefore sees either wholly the old pair or wholly the new one. Without a
// ledger there is only the pointer, and a single atomic load is already
// indivisible.
func (e *Engine) coherentPolicyAndLedger() (*enginePolicy, LedgerSnapshot) {
	if e.targetPermits == nil {
		return e.currentPolicy(), LedgerSnapshot{}
	}
	var p *enginePolicy
	conns := e.targetPermits.snapshotWith(func() { p = e.currentPolicy() })
	return p, conns
}

// setTargetConnBudget changes the aggregate budget on a running engine,
// touching the ledger and NOTHING ELSE.
//
// UNEXPORTED, because on its own it is half a policy change: no generation, no
// durability, no audit row, and no accompanying timeouts. That half used to be
// the whole of what a caller could do -- the budget moved, the bounds did not,
// and a restart forgot it. ReloadPolicy is the operator path and the only
// exported one; this is the piece it publishes through.
//
// It NEVER closes a socket. Lowering the budget below what is outstanding
// leaves every live session alone and simply stops granting new permits, so
// the number becomes true by attrition. Making it true immediately would turn
// a configuration edit into an outage.
func (e *Engine) setTargetConnBudget(n int) error {
	if e.targetPermits == nil {
		return fmt.Errorf("%w: this engine was built without a target connection budget",
			ErrInvalidBudget)
	}
	return e.targetPermits.SetBudget(n)
}
