package exec

import (
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

	// ResidentBudget is the memory front-door sessions may reserve in total;
	// zero means the guard is off. (was: LeaseCap beside it. The front-door
	// lease limit is each pool's front-door share: Pools.)
	ResidentBudget int64

	// PolicyGeneration counts published reloads. Zero is the configuration the
	// daemon started with.
	PolicyGeneration uint64

	// Pools is every connection pool's live ledger, by id. was: TargetConns,
	// the one instance-wide budget.
	Pools []PoolSnapshot
}

// Settings returns the effective configuration.
func (e *Engine) Settings() Settings {
	// The policy and the pools are separate facts now: a reload never moves a
	// budget, so the two need no shared lock. Each pool's reading is still
	// taken under its own ledger's lock (poolSet.snapshot).
	p := e.currentPolicy()
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
		ResidentBudget:       e.sessions.residentCap,
		PolicyGeneration:     p.generation,
		Pools:                e.pools.snapshot(),
	}
}
