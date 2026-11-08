package exec

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/timeouts"
)

// No policy timeout may exceed timeouts.MaxBound: a write above it is refused,
// a stored value above it is clamped on load.

// A RELOAD ABOVE THE BOUND IS REFUSED, NAMING THE ROW, and moves nothing.
func TestReloadPolicy_RefusesATimeoutAboveMaxBoundNamingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	before := f.eng.Settings()

	spec := goodSpec()
	spec.MaxTxDuration = 3 * time.Hour
	spec.MaxTxDurationCeiling = 3 * time.Hour
	_, err := f.eng.ReloadPolicy(ctx, f.rootTok, spec, testIP)
	if !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid for a 3h max_tx_duration", err)
	}
	if !strings.Contains(err.Error(), "max_tx_duration is 3h0m0s") {
		t.Errorf("the refusal does not name the row and its value: %v", err)
	}
	if got := f.eng.Settings(); !reflect.DeepEqual(got, before) {
		t.Errorf("a refused spec moved the live settings:\n before %+v\n after  %+v", before, got)
	}
	if _, ok, _ := f.store.GetMeta(ctx, policyStoreKey); ok {
		t.Error("a refused spec was stored")
	}
}

// storePolicyBlob writes a stored policy directly, as a release from before
// the bound would have left it.
func storePolicyBlob(t *testing.T, f *fixture, p storedPolicy) {
	t.Helper()
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMeta(context.Background(), policyStoreKey, string(blob)); err != nil {
		t.Fatalf("storing the policy: %v", err)
	}
}

// A STORED POLICY ABOVE THE BOUND STILL STARTS THE DAEMON: it runs clamped,
// is re-stored clamped under the next generation, and the clamp is audited
// once. The next start finds nothing to clamp.
func TestLoadDurablePolicy_ClampsAStoredPolicyAboveMaxBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := policyFixture(t)
	eightHours := (8 * time.Hour).Milliseconds()
	storePolicyBlob(t, f, storedPolicy{
		SessionIdleMS: (5 * time.Minute).Milliseconds(), IdleInTxMS: (90 * time.Minute).Milliseconds(),
		MaxTxMS: eightHours, MaxTxCeilingMS: eightHours, Generation: 4, UpdatedAt: 1, UpdatedBy: 1,
	})

	start := func() Settings {
		t.Helper()
		eng := New(f.store, f.svc, WithMaxRows(3))
		t.Cleanup(func() { _ = eng.Close() })
		if err := eng.LoadDurablePolicy(ctx); err != nil {
			t.Fatalf("LoadDurablePolicy refused a stored policy it should clamp: %v", err)
		}
		return eng.Settings()
	}

	got := start()
	if got.MaxTxDuration != timeouts.MaxBound || got.MaxTxDurationCeiling != timeouts.MaxBound {
		t.Errorf("max tx %v, ceiling %v; want both clamped to %v",
			got.MaxTxDuration, got.MaxTxDurationCeiling, timeouts.MaxBound)
	}
	if got.IdleInTxTimeout != 90*time.Minute || got.SessionIdleTimeout != 5*time.Minute {
		t.Errorf("rows within the bound changed: idle in tx %v, session idle %v", got.IdleInTxTimeout, got.SessionIdleTimeout)
	}
	if got.PolicyGeneration != 5 {
		t.Errorf("generation = %d, want 5: the clamped policy is a new publication", got.PolicyGeneration)
	}

	raw, ok, err := f.store.GetMeta(ctx, policyStoreKey)
	if err != nil || !ok {
		t.Fatalf("reading the stored policy: ok=%v err=%v", ok, err)
	}
	var stored storedPolicy
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 5 || stored.MaxTxMS != timeouts.MaxBound.Milliseconds() ||
		stored.MaxTxCeilingMS != timeouts.MaxBound.Milliseconds() {
		t.Errorf("the stored policy was not re-written clamped under generation 5: %+v", stored)
	}

	rows := auditDetail(t, f, "policy_clamped")
	if len(rows) != 1 {
		t.Fatalf("policy_clamped rows = %d, want 1", len(rows))
	}
	for _, want := range []string{"generation 4 -> 5", "max_tx_duration 8h0m0s -> 2h0m0s",
		"max_tx_duration_ceiling 8h0m0s -> 2h0m0s"} {
		if !strings.Contains(rows[0], want) {
			t.Errorf("the policy_clamped row is missing %q:\n%s", want, rows[0])
		}
	}

	// The re-stored policy is within the bound, so a second start clamps
	// nothing and writes nothing.
	if again := start(); again.PolicyGeneration != 5 {
		t.Errorf("a second start moved the generation to %d", again.PolicyGeneration)
	}
	if n := len(auditDetail(t, f, "policy_clamped")); n != 1 {
		t.Errorf("policy_clamped rows after a second start = %d, want 1", n)
	}
}

// A CLAMPED POLICY THAT CANNOT BE RE-STORED still runs, under the STORED
// generation, so the next start's generation names the blob it writes.
func TestLoadDurablePolicy_AFailedRewriteRunsUnderTheStoredGeneration(t *testing.T) {
	t.Parallel()
	f := policyFixture(t)
	failing, cancel := context.WithCancel(context.Background())
	cancel()

	got := f.eng.persistClampedPolicy(failing, goodSpec(), 4, []string{"max_tx_duration 8h0m0s -> 2h0m0s"})
	if got != 4 {
		t.Errorf("generation = %d after a failed write, want the stored 4", got)
	}
	if n := len(auditDetail(t, f, "policy_clamped")); n != 0 {
		t.Errorf("a failed write left %d policy_clamped rows; the row and the blob go together", n)
	}
	if _, ok, _ := f.store.GetMeta(context.Background(), policyStoreKey); ok {
		t.Error("a failed write stored a policy")
	}
}
