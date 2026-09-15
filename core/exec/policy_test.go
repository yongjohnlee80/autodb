package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The reload: the one path that changes the bounds on a running daemon.
//
// What it replaces changed the budget alone, from inside the process, and
// forgot it on restart. Each cell below is one of the properties that absence
// cost: authorization, validation before mutation, durability, and a single
// coherent publication.

func goodSpec() PolicySpec {
	return PolicySpec{
		SessionIdleTimeout:   5 * time.Minute,
		IdleInTxTimeout:      90 * time.Minute,
		MaxTxDuration:        6 * time.Hour,
		MaxTxDurationCeiling: 8 * time.Hour,
		MaxTargetConns:       40,
	}
}

// policyFixture is a fixture whose engine carries a connection budget, so the
// ledger half of the publication is exercised rather than skipped.
func policyFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	// The fixture's engine is built without a ledger; replace it with one
	// built the way a front-door daemon builds it.
	old := f.eng
	f.eng = New(f.store, f.svc, WithMaxRows(3), WithTargetConnBudget(25))
	t.Cleanup(func() { _ = f.eng.Close() })
	_ = old.Close()
	return f
}

// ONLY AN ADMIN. The bounds decide how long anyone may hold a production
// connection; that is an administrative act, not a user preference.
func TestReloadPolicy_RefusesAnyoneWhoIsNotAnAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	if _, err := f.svc.CreateUser(ctx, f.rootTok, "editor1", "editor-passphrase", meta.RoleEditor, testIP); err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.Login(ctx, "editor1", "editor-passphrase", testIP)
	if err != nil {
		t.Fatal(err)
	}

	before := f.eng.Settings()
	if _, err := f.eng.ReloadPolicy(ctx, tok, goodSpec(), testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("an editor's reload returned %v, want ErrDenied", err)
	}
	if _, err := f.eng.ReloadPolicy(ctx, "not-a-token", goodSpec(), testIP); err == nil {
		t.Error("an unauthenticated reload succeeded")
	}
	if got := f.eng.Settings(); got != before {
		t.Errorf("a refused reload changed the live settings:\n before %+v\n after  %+v", before, got)
	}
	if n := f.auditCount(t, "policy_reloaded"); n != 0 {
		t.Errorf("%d policy_reloaded rows after two refusals; the trail claims a change that "+
			"never happened", n)
	}
	if _, ok, _ := f.store.GetMeta(ctx, policyStoreKey); ok {
		t.Error("a refused reload wrote a durable policy")
	}
}

// VALIDATED WHOLE, BEFORE ANYTHING MOVES. A reload that applied fields as it
// checked them would leave the engine half on the operator's policy and half
// on the old one, with no record of which.
func TestReloadPolicy_AnInvalidSpecChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	before := f.eng.Settings()

	for _, c := range []struct {
		name string
		spec PolicySpec
	}{
		{"a zero duration", PolicySpec{IdleInTxTimeout: time.Hour, MaxTxDuration: time.Hour,
			MaxTxDurationCeiling: time.Hour, MaxTargetConns: 25}},
		{"a max above the ceiling", func() PolicySpec {
			s := goodSpec()
			s.MaxTxDuration = 9 * time.Hour
			return s
		}()},
		{"a budget of one", func() PolicySpec {
			s := goodSpec()
			s.MaxTargetConns = 1
			return s
		}()},
		{"a session idle longer than the pool's", func() PolicySpec {
			s := goodSpec()
			s.SessionIdleTimeout = 11 * time.Minute // the default pool idle is 10m
			return s
		}()},
	} {
		if _, err := f.eng.ReloadPolicy(ctx, f.rootTok, c.spec, testIP); !errors.Is(err, ErrPolicyInvalid) {
			t.Errorf("%s: err = %v, want ErrPolicyInvalid", c.name, err)
		}
	}

	if got := f.eng.Settings(); got != before {
		t.Errorf("a refused spec moved the live settings:\n before %+v\n after  %+v", before, got)
	}
	if _, ok, _ := f.store.GetMeta(ctx, policyStoreKey); ok {
		t.Error("a refused spec was written to the store")
	}
}

// THE JANITOR'S INTERVAL BOUNDS THE RELOAD, not the other way round. A
// deadline shorter than the gap between sweeps is a deadline nothing looks at
// in time.
func TestReloadPolicy_RefusesADeadlineTheSweepCouldNotEnforce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.eng.StartJanitor(sweepCtx, 2*time.Minute)

	spec := goodSpec()
	spec.IdleInTxTimeout = time.Minute
	_, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
	if !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid for an idle bound below the sweep interval", err)
	}
	if !strings.Contains(err.Error(), "janitor interval") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// ONE PUBLICATION. The timeouts and the budget arrive together, the generation
// advances, and the deprecated debug bound is derived rather than supplied.
func TestReloadPolicy_PublishesTheBoundsAsOneGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	if g := f.eng.Settings().PolicyGeneration; g != 0 {
		t.Fatalf("a freshly built engine is at generation %d, want 0 — the configured policy", g)
	}

	got, err := f.eng.ReloadPolicy(ctx, f.rootTok, goodSpec(), testIP)
	if err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"generation", got.PolicyGeneration, uint64(1)},
		{"session idle", got.SessionIdleTimeout, 5 * time.Minute},
		{"idle in tx", got.IdleInTxTimeout, 90 * time.Minute},
		{"max tx", got.MaxTxDuration, 6 * time.Hour},
		{"ceiling", got.MaxTxDurationCeiling, 8 * time.Hour},
		{"budget", got.TargetConns.Configured, 40},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// DERIVED, so the two cannot disagree. A stored debug bound differing from
	// the common one would describe behaviour the runtime does not have.
	if got.DebugIdleInTxTimeout != got.IdleInTxTimeout {
		t.Errorf("debug idle %v differs from the common idle bound %v; the deprecated "+
			"profile is being allowed to select something again",
			got.DebugIdleInTxTimeout, got.IdleInTxTimeout)
	}
	// A second reload advances again, from the new base.
	second := goodSpec()
	second.MaxTargetConns = 30
	if got, err := f.eng.ReloadPolicy(ctx, f.rootTok, second, testIP); err != nil || got.PolicyGeneration != 2 {
		t.Errorf("second reload: generation %d err %v, want 2 and nil", got.PolicyGeneration, err)
	}
}

// THE BOUNDS ARE ACTUALLY IN FORCE, not merely reported. A readout that agreed
// with the operator while the sweep used the old numbers would be the worst of
// both.
func TestReloadPolicy_TheNewBoundsAreWhatTheSweepEnforces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, s := holderFixture(t, 2*time.Hour, 8*time.Hour)

	// The session's OWN limits were resolved when its transaction opened, so
	// what the reload must change is what a NEW transaction gets -- and the
	// session idle bound, which is read fresh on every sweep.
	spec := goodSpec()
	spec.SessionIdleTimeout = time.Minute
	if _, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP); err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}

	// No transaction, idle past the new one-minute bound.
	s.mu.Lock()
	s.txPhase, s.txID = txNone, ""
	s.lastUsed = time.Now().Add(-90 * time.Second)
	s.mu.Unlock()

	if n := f.eng.reapExpired(ctx, time.Now()); n != 1 {
		t.Fatalf("the sweep acted on %d sessions, want 1 — it is still reading the "+
			"configured session idle bound rather than the reloaded one", n)
	}
}

// IT SURVIVES A RESTART. This is the property the old setter did not have, and
// the reason an operator's 3am change used to be undone by the next deploy.
func TestReloadPolicy_SurvivesARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)

	if _, err := f.eng.ReloadPolicy(ctx, f.rootTok, goodSpec(), testIP); err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}

	// A NEW ENGINE over the SAME store, built from the same configured
	// defaults a restarted daemon would use.
	restarted := New(f.store, f.svc, WithMaxRows(3), WithTargetConnBudget(25))
	t.Cleanup(func() { _ = restarted.Close() })
	if got := restarted.Settings(); got.IdleInTxTimeout == 90*time.Minute {
		t.Fatal("the fresh engine already has the reloaded bound before loading it, so this " +
			"cell cannot tell durability from a shared pointer")
	}
	if err := restarted.LoadDurablePolicy(ctx); err != nil {
		t.Fatalf("LoadDurablePolicy: %v", err)
	}

	got := restarted.Settings()
	if got.IdleInTxTimeout != 90*time.Minute || got.MaxTxDuration != 6*time.Hour ||
		got.SessionIdleTimeout != 5*time.Minute || got.MaxTxDurationCeiling != 8*time.Hour {
		t.Errorf("the restarted engine is running the configured policy, not the reloaded one: %+v", got)
	}
	if got.TargetConns.Configured != 40 {
		t.Errorf("budget after restart = %d, want the reloaded 40", got.TargetConns.Configured)
	}
	if got.PolicyGeneration != 1 {
		t.Errorf("generation after restart = %d, want the stored 1 — a restart that reset the "+
			"counter would make two different policies indistinguishable to an observer",
			got.PolicyGeneration)
	}
}

// A STORED POLICY THAT NO LONGER VALIDATES STOPS THE DAEMON rather than being
// silently discarded.
func TestLoadDurablePolicy_RefusesAStoredPolicyThatNoLongerValidates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	if _, err := f.eng.ReloadPolicy(ctx, f.rootTok, goodSpec(), testIP); err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}

	// The pool's idle time is lowered underneath the stored session bound --
	// exactly the kind of change that happens in a config file between
	// deploys.
	restarted := New(f.store, f.svc, WithPoolLimits(0, time.Minute, 0), WithTargetConnBudget(25))
	t.Cleanup(func() { _ = restarted.Close() })
	err := restarted.LoadDurablePolicy(ctx)
	if !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid — a stored policy that cannot be honoured "+
			"must stop the daemon, not be dropped in favour of bounds nobody chose", err)
	}

	// And nothing was half-applied on the way to the refusal.
	if got := restarted.Settings(); got.SessionIdleTimeout == 5*time.Minute {
		t.Error("the refused stored policy was partially published")
	}
}

// THE AUDIT ROW says what changed, from what, and carries no credential.
func TestReloadPolicy_AuditsTheChangeWithoutSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	if _, err := f.eng.ReloadPolicy(ctx, f.rootTok, goodSpec(), testIP); err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}

	rows := auditDetail(t, f, "policy_reloaded")
	if len(rows) != 1 {
		t.Fatalf("policy_reloaded rows = %d, want 1", len(rows))
	}
	detail := rows[0]
	// BEFORE AS WELL AS AFTER. "set to 90m" does not tell a reader whether
	// anything actually changed.
	for _, want := range []string{"generation 0 -> 1", "2h0m0s -> 1h30m0s", "max_target_conns -> 40"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the audit detail is missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, f.rootTok) {
		t.Error("the operator's token is in the audit detail")
	}
}

// A BOUND MUST RUN AT THE VALUE IT PERSISTS AS.
//
// The durable policy keeps milliseconds. A finer bound would run at the value
// given and store as something else: a 1ns bound persists as zero, and the next
// start refuses to boot on a policy the operator never wrote. It is refused
// rather than rounded, because rounding means the daemon runs a bound nobody
// chose while the audit row records the one they asked for.
func TestReloadPolicy_RefusesADurationItCannotPersistExactly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	before := f.eng.Settings()

	for _, c := range []struct {
		name string
		mut  func(*PolicySpec)
	}{
		{"a sub-millisecond idle bound", func(s *PolicySpec) { s.IdleInTxTimeout = 90*time.Minute + time.Nanosecond }},
		{"a sub-millisecond session bound", func(s *PolicySpec) { s.SessionIdleTimeout = 5*time.Minute + 500*time.Microsecond }},
		{"a sub-millisecond maximum", func(s *PolicySpec) { s.MaxTxDuration = 6*time.Hour + time.Microsecond }},
	} {
		spec := goodSpec()
		c.mut(&spec)
		_, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
		if !errors.Is(err, ErrPolicyInvalid) {
			t.Errorf("%s: err = %v, want ErrPolicyInvalid", c.name, err)
		}
	}

	// NOTHING MOVED. The refusal happens before the store, the audit row and
	// the generation, so a rejected duration leaves no trace but the error.
	if got := f.eng.Settings(); got != before {
		t.Error("a refused duration changed the live settings")
	}
	if n := f.auditCount(t, "policy_reloaded"); n != 0 {
		t.Errorf("%d policy_reloaded rows after three refusals", n)
	}

	// AND THE ACCEPTED VALUES ROUND-TRIP EXACTLY. A whole number of
	// milliseconds is what the durable form keeps, so what comes back after a
	// restart is bit-for-bit what was asked for.
	spec := goodSpec()
	spec.IdleInTxTimeout = 90*time.Minute + 3*time.Millisecond
	got, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
	if err != nil {
		t.Fatalf("a millisecond-aligned bound was refused: %v", err)
	}
	if got.IdleInTxTimeout != spec.IdleInTxTimeout {
		t.Errorf("idle = %v, want %v", got.IdleInTxTimeout, spec.IdleInTxTimeout)
	}
	restarted := New(f.store, f.svc, WithMaxRows(3), WithTargetConnBudget(25))
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.LoadDurablePolicy(ctx); err != nil {
		t.Fatalf("LoadDurablePolicy: %v", err)
	}
	if v := restarted.Settings().IdleInTxTimeout; v != spec.IdleInTxTimeout {
		t.Errorf("after restart idle = %v, want %v — the stored form lost precision", v, spec.IdleInTxTimeout)
	}
}
