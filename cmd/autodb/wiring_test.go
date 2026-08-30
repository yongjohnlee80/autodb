package main

import (
	"context"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
)

// MF3. Every [exec] setting has to REACH the engine. All of them were parsed,
// validated and defaulted, and then dropped: the construction site passed
// history and max_statement_bytes and nothing else, so an operator who set
// max_tx_duration got the built-in default with no indication their value had
// gone nowhere.
//
// The values below are deliberately unlike every default. A test using the
// defaults would pass against a call site that passed no options at all,
// which is the version of this test that would have shipped the bug.
func TestExecOptions_EveryConfiguredValueReachesTheEngine(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Exec = config.Exec{
		MaxStatementBytes:    4242,
		MaxSessionsPerUser:   3,
		MaxSessionsGlobal:    77,
		SessionIdleTimeout:   config.Duration(11 * time.Minute),
		IdleInTxTimeout:      config.Duration(13 * time.Second),
		MaxTxDuration:        config.Duration(14 * time.Minute),
		DebugIdleInTxTimeout: config.Duration(17 * time.Minute),
		MaxTxDurationCeiling: config.Duration(19 * time.Minute),
		PoolMaxConns:         6,
		PoolMaxConnIdleTime:  config.Duration(21 * time.Second),
		PoolMaxConnLifetime:  config.Duration(23 * time.Minute),
		JanitorInterval:      config.Duration(2 * time.Second),
	}

	eng := coreexec.New(nil, nil, execOptions(cfg, func(string) {})...)
	got := eng.Settings()

	for _, c := range []struct {
		name       string
		got, want  any
		wasDropped string
	}{
		{"max_statement_bytes", got.MaxStatementBytes, 4242, ""},
		{"max_sessions_per_user", got.MaxSessionsPerUser, 3, "the per-user cap was unreachable"},
		{"max_sessions_global", got.MaxSessionsGlobal, 77, "the global cap was unreachable"},
		{"session_idle_timeout", got.SessionIdleTimeout, 11 * time.Minute, "orphaned sessions were reaped on the default schedule"},
		{"idle_in_tx_timeout", got.IdleInTxTimeout, 13 * time.Second, "an abandoned transaction held locks for the default 90s"},
		{"max_tx_duration", got.MaxTxDuration, 14 * time.Minute, "the outside bound on a transaction was the default"},
		{"debug_idle_in_tx_timeout", got.DebugIdleInTxTimeout, 17 * time.Minute, ""},
		{"max_tx_duration_ceiling", got.MaxTxDurationCeiling, 19 * time.Minute, ""},
		{"pool_max_conns", got.PoolMaxConns, 6, "a target's connection budget was bounded by the default"},
		{"pool_max_conn_idle_time", got.PoolMaxConnIdleTime, 21 * time.Second, ""},
		{"pool_max_conn_lifetime", got.PoolMaxConnLifetime, 23 * time.Minute, ""},
	} {
		if c.got != c.want {
			msg := c.wasDropped
			if msg == "" {
				msg = "the configured value never reached the engine"
			}
			t.Errorf("[exec] %s = %v, want %v — %s", c.name, c.got, c.want, msg)
		}
	}
}

// MF2. A lost lease is a shutdown. Nothing read Lost() before, so an engine
// whose lock had dropped kept serving — and a second engine, finding the lock
// free, would take it. That is the two-engines-one-store state the lease
// exists to prevent, reached through the lease itself.
func TestWatchLease_LosingTheLeaseStopsServing(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	stopped := make(chan struct{})
	var warned string
	fired := watchLease(t.Context(), lost, func() { close(stopped) }, func(m string) { warned = m })

	// Positive control: while the lease holds, nothing stops. Without this a
	// watcher that stopped unconditionally would pass the assertion below.
	select {
	case <-stopped:
		t.Fatal("serving stopped while the lease was still held")
	case <-time.After(20 * time.Millisecond):
	}

	close(lost)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the lease was lost and the daemon kept serving; a second engine can now take the " +
			"lock while this one is still writing to the same meta store")
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the loss was not reported, so the exit is indistinguishable from a clean shutdown")
	}
	if warned == "" {
		t.Error("nothing was logged; the operator has no way to learn why the daemon stopped")
	}
}

// And the watcher must not outlive the daemon: a goroutine per serve that
// never returns is a leak in every test binary that starts one.
func TestWatchLease_StopsWithTheServeContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	watchLease(ctx, make(chan struct{}), func() { close(stopped) }, func(string) {})
	cancel()
	select {
	case <-stopped:
		t.Fatal("a cancelled serve context triggered the lease-loss path; shutdown would report a " +
			"lease failure that never happened")
	case <-time.After(50 * time.Millisecond):
	}
}
