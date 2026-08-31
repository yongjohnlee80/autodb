package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"

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

// MF2. The previous cells exercised the helpers directly, so deleting the
// production calls to watchLease and StartJanitor left the whole suite green
// — which is the failure mode the helpers were supposed to prevent. These go
// through startEngine, the one place the daemon wires them, and observe the
// EFFECT rather than the call.
func startedEngine(t *testing.T, cfg config.Config, lost <-chan struct{}) (*coreexec.Engine, context.Context, *meta.Store, *auth.Service, string) {
	t.Helper()

	store, err := meta.Open(t.Context(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok, _, err := svc.Bootstrap(t.Context(), "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	eng, serveCtx, _, stop := startEngine(t.Context(), cfg, store, svc, lost, func(string) {})
	t.Cleanup(func() { stop(); _ = eng.Close() })
	return eng, serveCtx, store, svc, tok
}

func execConfig() config.Config {
	cfg := config.Config{}
	cfg.Exec = config.Exec{
		MaxStatementBytes:    65536,
		MaxSessionsPerUser:   8,
		MaxSessionsGlobal:    256,
		SessionIdleTimeout:   config.Duration(50 * time.Millisecond),
		IdleInTxTimeout:      config.Duration(90 * time.Second),
		MaxTxDuration:        config.Duration(5 * time.Minute),
		DebugIdleInTxTimeout: config.Duration(10 * time.Minute),
		MaxTxDurationCeiling: config.Duration(30 * time.Minute),
		PoolMaxConns:         8,
		PoolMaxConnIdleTime:  config.Duration(10 * time.Minute),
		PoolMaxConnLifetime:  config.Duration(60 * time.Minute),
		JanitorInterval:      config.Duration(20 * time.Millisecond),
		ReconcileInterval:    config.Duration(20 * time.Millisecond),
	}
	return cfg
}

// config → daemon → an actual reconciliation pass.
//
// Same MF2 reasoning as the janitor cell: the reconciler is exercised
// directly in core/exec, so deleting the production call in startEngine would
// leave every one of those tests green while the daemon kept a complete
// record of undetermined transactions and never went back to find out. This
// goes through startEngine and observes the EFFECT.
//
// The entry is seeded against a sqlite target, which has no oracle, so the
// pass terminates it outcome_unresolvable(no-oracle) without needing a live
// PostgreSQL — the property under test is that the daemon RUNS the
// reconciler, not what the reconciler concludes.
func TestStartEngine_TheReconcilerActuallyRunsOnTheConfiguredSchedule(t *testing.T) {
	t.Parallel()

	seedPending := func(t *testing.T, store *meta.Store, connID int64, txID string) {
		t.Helper()
		for i, st := range []string{"opened", "commit_started"} {
			if _, err := store.TxOutcomes.OnCtx(t.Context()).
				Set(meta.TxOutTxID, txID).Set(meta.TxOutSeq, int64(i+1)).
				Set(meta.TxOutState, st).Set(meta.TxOutReason, "").
				Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, connID).
				Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "55").
				Set(meta.TxOutCreatedAt, int64(1000+i)).Insert(); err != nil {
				t.Fatalf("seeding: %v", err)
			}
		}
	}
	settled := func(t *testing.T, store *meta.Store, txID string) bool {
		t.Helper()
		rows, err := store.TxOutcomes.OnCtx(t.Context()).With(meta.TxOutTxID, txID).Select()
		if err != nil {
			t.Fatalf("reading the log: %v", err)
		}
		for _, r := range rows {
			if r.State == "committed" || r.State == "rolled_back" || r.State == "outcome_unresolvable" {
				return true
			}
		}
		return false
	}

	cfg := execConfig()
	eng, _, store, _, tok := startedEngine(t, cfg, make(chan struct{}))
	dsn := fmt.Sprintf("file:recon%d?mode=memory&cache=shared", time.Now().UnixNano())
	connID, err := eng.CreateConnection(t.Context(), tok, "target", "sqlite", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	// Positive control: an `opened` transaction is NOT the reconciler's to
	// settle, so if this one were also resolved the test below would be
	// satisfied by a pass that terminates everything it sees.
	seedControl := func() {
		if _, err := store.TxOutcomes.OnCtx(t.Context()).
			Set(meta.TxOutTxID, "tx_control").Set(meta.TxOutSeq, int64(1)).
			Set(meta.TxOutState, "opened").Set(meta.TxOutReason, "").
			Set(meta.TxOutUserID, int64(1)).Set(meta.TxOutConnID, connID).
			Set(meta.TxOutHistoryID, int64(0)).Set(meta.TxOutTargetXID, "").
			Set(meta.TxOutCreatedAt, int64(1)).Insert(); err != nil {
			t.Fatalf("seeding the control: %v", err)
		}
	}
	seedControl()
	seedPending(t, store, connID, "tx_pending")

	// 25x the configured interval. Nothing in this test calls the
	// reconciler.
	time.Sleep(500 * time.Millisecond)

	if !settled(t, store, "tx_pending") {
		t.Fatal("an undetermined transaction outlived 25 reconcile intervals: the daemon either " +
			"never started the reconciler or never passed it the configured interval, so a " +
			"crash-window outcome would stay unknown forever")
	}
	if settled(t, store, "tx_control") {
		t.Fatal("an `opened` transaction was terminated too — the pass settles everything it " +
			"sees, so the assertion above proves nothing about reconciliation")
	}
}

// config → daemon → an actual reap. The session idle timeout and the janitor
// interval both come from configuration and nothing in the test calls the
// reaper: if the daemon does not start the janitor, or does not pass the
// configured timings, the session simply never goes away.
func TestStartEngine_TheJanitorActuallyReapsOnTheConfiguredSchedule(t *testing.T) {
	t.Parallel()

	openSession := func(t *testing.T, cfg config.Config) (*coreexec.Engine, coreexec.SessionID, string) {
		t.Helper()
		eng, _, store, _, tok := startedEngine(t, cfg, make(chan struct{}))
		dsn := fmt.Sprintf("file:wiring%d?mode=memory&cache=shared", time.Now().UnixNano())
		connID, err := eng.CreateConnection(t.Context(), tok, "target", "sqlite", dsn, "127.0.0.1")
		if err != nil {
			t.Fatalf("CreateConnection: %v", err)
		}
		if err := store.Connections.OnCtx(t.Context()).With(meta.ConnID, connID).
			Set(meta.ConnProfile, string(coreexec.ProfileSession)).Update(); err != nil {
			t.Fatalf("enabling the session profile: %v", err)
		}
		sid, err := eng.OpenSession(t.Context(), tok, connID, "127.0.0.1")
		if err != nil {
			t.Fatalf("OpenSession: %v", err)
		}
		return eng, sid, tok
	}

	// Positive control: with a long idle timeout the session SURVIVES.
	// Without it, a test asserting the session goes away would be satisfied
	// by a session that could never be opened or that died for any reason.
	t.Run("a session within its idle timeout survives", func(t *testing.T) {
		t.Parallel()
		cfg := execConfig()
		cfg.Exec.SessionIdleTimeout = config.Duration(time.Hour)
		eng, sid, tok := openSession(t, cfg)
		time.Sleep(200 * time.Millisecond)
		if _, err := eng.SessionExecute(t.Context(), tok, sid, "SELECT 1", "127.0.0.1"); errors.Is(err, coreexec.ErrSessionNotFound) {
			t.Fatal("a session well within its idle timeout was reaped")
		}
	})

	t.Run("an idle session is reaped without anyone calling the reaper", func(t *testing.T) {
		t.Parallel()
		eng, sid, tok := openSession(t, execConfig())

		// Checked ONCE, after waiting. Polling with SessionExecute would
		// keep the session alive — every statement refreshes lastUsed — so a
		// poll loop asks a question whose own asking prevents the answer.
		// The wait is 10x the idle timeout and 25x the janitor interval.
		time.Sleep(500 * time.Millisecond)
		_, err := eng.SessionExecute(t.Context(), tok, sid, "SELECT 1", "127.0.0.1")
		if !errors.Is(err, coreexec.ErrSessionNotFound) {
			t.Fatalf("the session outlived its configured idle timeout by an order of magnitude "+
				"(SessionExecute = %v): the daemon either never started the janitor or never "+
				"passed it the configured timings, so nothing enforces the bounds an "+
				"operator set", err)
		}
	})
}

// lease loss → the server context actually stops. Not the helper in
// isolation: the context startEngine hands the server.
func TestStartEngine_LosingTheLeaseStopsTheServeContext(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	_, serveCtx, _, _, _ := startedEngine(t, execConfig(), lost)

	// Positive control: the server runs while the lease holds.
	select {
	case <-serveCtx.Done():
		t.Fatal("the serve context was cancelled while the lease was still held")
	case <-time.After(50 * time.Millisecond):
	}

	close(lost)
	select {
	case <-serveCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lease was lost and the server kept running; a second engine can now take " +
			"the lock while this one is still writing to the same meta store")
	}
}
