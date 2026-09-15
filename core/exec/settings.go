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

	// TargetConns is the aggregate production-connection budget and its live
	// state. Zero Configured means no budget is in force, which is what an
	// engine embedded without a front door gets.
	TargetConns LedgerSnapshot
}

// Settings returns the effective configuration.
func (e *Engine) Settings() Settings {
	return Settings{
		MaxStatementBytes:    e.maxStatementBytes,
		MaxSessionsPerUser:   e.sessions.perUserCap,
		MaxSessionsGlobal:    e.sessions.globalCap,
		SessionIdleTimeout:   e.sessionIdle,
		IdleInTxTimeout:      e.txLimits.idleInTx,
		MaxTxDuration:        e.txLimits.maxTx,
		DebugIdleInTxTimeout: e.debugIdle,
		MaxTxDurationCeiling: e.maxTxCeiling,
		PoolMaxConns:         e.poolMaxConns,
		PoolMaxConnIdleTime:  e.poolMaxConnIdleTime,
		PoolMaxConnLifetime:  e.poolMaxConnLifetime,
		LeaseCap:             e.sessions.leaseCap,
		ResidentBudget:       e.sessions.residentCap,
		TargetConns:          e.targetConnSnapshot(),
	}
}

// targetConnSnapshot reads the ledger, or reports an absent budget.
func (e *Engine) targetConnSnapshot() LedgerSnapshot {
	if e.targetPermits == nil {
		return LedgerSnapshot{}
	}
	return e.targetPermits.Snapshot()
}

// SetTargetConnBudget changes the aggregate budget on a running engine.
//
// NO OPERATOR SURFACE CALLS THIS YET, and that is stated rather than implied.
// There is no reload path or admin method in this daemon to route it through,
// so today it is reachable only from a program embedding the engine. Calling
// it a "live update path" without that caveat would describe a capability an
// operator does not have.
//
// It is here rather than deferred because the ledger's drain semantics are
// what make a lowered budget safe, and they had no exercisable entry point at
// all -- SetBudget was reachable only from tests, so the behaviour existed
// without any way for the thing it was written for to reach it.
//
// THE PRODUCTION UPDATE PATH. Until this existed the budget could only be set
// at construction and SetBudget was reachable only from tests — so the drain
// semantics the ledger implements had no way to be exercised by the thing they
// were written for.
//
// It NEVER closes a socket. Lowering the budget below what is outstanding
// leaves every live session alone and simply stops granting new permits, so
// the number becomes true by attrition. Making it true immediately would turn
// a configuration edit into an outage.
func (e *Engine) SetTargetConnBudget(n int) error {
	if e.targetPermits == nil {
		return fmt.Errorf("%w: this engine was built without a target connection budget",
			ErrInvalidBudget)
	}
	return e.targetPermits.SetBudget(n)
}
