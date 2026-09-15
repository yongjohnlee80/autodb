package rpc_test

import (
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

// THE OPERATOR SURFACE EXISTS AND IS REACHABLE.
//
// The engine's reload was written before anything could call it, which is the
// state the policy called out by name: a setter reachable only from a program
// embedding the engine is not an operator surface. A handler nothing exercises
// is the same thing one step further along -- it compiles, it is registered,
// and nobody finds out it was never wired until an incident.
func TestPolicyVerbs_AreReachableOverTheWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// READ: any authenticated caller. An operator asking why their
	// transaction ended needs to see the bound that ended it.
	errVal, result := c.call("policy.show", f.rootTok)
	if errVal != nil {
		t.Fatalf("policy.show: %#v", errVal)
	}
	before := result.(map[string]any)
	for _, key := range []string{
		"generation", "session_idle_ms", "idle_in_tx_ms", "max_tx_ms",
		"max_tx_ceiling_ms", "max_target_conns",
	} {
		if _, ok := before[key]; !ok {
			t.Errorf("policy.show omits %q", key)
		}
	}

	// WRITE: admin only.
	if errVal, _ = c.call("auth.user_create", f.rootTok, "policy-editor", "editor-passphrase", "editor"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	editorTok := c.login("policy-editor", "editor-passphrase")
	errVal, _ = c.call("policy.reload", editorTok, 300000, 5400000, 21600000, 28800000, 40)
	mustErr(t, errVal, rpc.CodeDenied)

	// An invalid spec is refused ON THE WIRE, not accepted and then ignored.
	errVal, _ = c.call("policy.reload", f.rootTok, 300000, 5400000, 32400000, 28800000, 40)
	if errVal == nil {
		t.Error("a max_tx_duration above the ceiling was accepted over the wire")
	}

	errVal, result = c.call("policy.reload", f.rootTok, 300000, 5400000, 21600000, 28800000, 40)
	if errVal != nil {
		t.Fatalf("policy.reload: %#v", errVal)
	}
	after := result.(map[string]any)
	if num(t, after["idle_in_tx_ms"]) != 5400000 {
		t.Errorf("idle_in_tx_ms = %#v, want 5400000 — the reload returned a view that does "+
			"not reflect what was asked for", after["idle_in_tx_ms"])
	}
	if num(t, after["generation"]) == num(t, before["generation"]) {
		t.Errorf("generation did not advance: %#v", after["generation"])
	}

	// And the READ agrees with the write, which is what an operator checks.
	if errVal, result = c.call("policy.show", f.rootTok); errVal != nil {
		t.Fatalf("policy.show after reload: %#v", errVal)
	}
	if got := result.(map[string]any); num(t, got["idle_in_tx_ms"]) != num(t, after["idle_in_tx_ms"]) ||
		num(t, got["generation"]) != num(t, after["generation"]) {
		t.Errorf("policy.show disagrees with the reload it just performed:\n show   %#v\n reload %#v",
			got, after)
	}
	if f.auditCount(t, "policy_reloaded") != 1 {
		t.Error("the administrative reload was not audited exactly once")
	}
}

// num reads a wire number whatever integer or float shape the codec chose,
// so the cell asserts the VALUE rather than the encoding.
func num(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	t.Fatalf("not a number on the wire: %#v", v)
	return 0
}
