package exec

import "time"

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
	}
}
