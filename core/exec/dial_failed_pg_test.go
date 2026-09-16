package exec

import (
	"context"
	"testing"
)

// A BACKEND THIS PACKAGE COULD NOT SANITISE MUST AUDIT AS A SETTINGS FAILURE,
// AND MUST COST NO MORE THAN THE RE-ARBITRATION BOUND ALLOWS.
//
// The stage is the operator's whole repair instruction. Every dial failure
// renders to the client as the same fixed literal, so the trail is the only
// place the difference between a name that will not resolve, a certificate that
// expired and a backend whose session state would not clear can be read. A
// sanitation failure carries no marker of its own: its cause is whatever the
// refused reset step reported, which is ordinary target text the stage
// classifier has no rule for and files as unclassified. An operator reading
// "unclassified" goes and tests the network, finds it healthy, and learns
// nothing about the backend that actually refused.
//
// THE OBVIOUS ALTERNATIVE — letting the classifier attribute the stage, the way
// it does for every other cause — cannot work here and that is why the raise
// site names the stage explicitly. The classifier reads driver types; this
// failure is autodb's own verdict about a connection the driver considers
// perfectly healthy.
//
// LIVE, because the refusal has to come from a real server. A fake that returns
// an error on demand would prove the wrap and nothing about the path: the
// checkout proof only runs against a backend that was really taken out of a
// really shared pool.
func TestDialFailedPG_ABackendThatCannotBeSanitisedAuditsAsSettings(t *testing.T) {
	lt := newLeakTarget(t)
	ctx := context.Background()

	// Open once so the target pool exists and is warm. This cell is about the
	// acquisition, not about opening a pool, and a cold pool would let an open
	// failure masquerade as a sanitation failure.
	b := lt.open(t)
	s := b.sess

	// Detach and destroy the pinned backend, so the acquisition below has to
	// take a fresh member. The target admits exactly one physical connection,
	// so this also frees the only slot.
	pc := s.pinnedConn()
	s.mu.Lock()
	s.pc = nil
	s.mu.Unlock()
	discardBackend(ctx, pc)

	// A reset step no server can run, installed AFTER the open: the same plan
	// runs when a session TAKES a backend, so a broken plan up front would
	// simply stop the session opening and this cell would observe nothing.
	lt.f.eng.backendReset = append(backendResetPlan(),
		resetStep{"a state no server can discard", "DISCARD NOTHING_LIKE_THIS"})
	t.Cleanup(func() { lt.f.eng.backendReset = nil })

	_, err := lt.f.eng.acquireRequestBackend(ctx, s, lt.row)
	d, ok := DialFailureOf(err)
	if !ok {
		t.Fatalf("a backend that could not be sanitised returned %v unframed; the front "+
			"door publishes an unrecognised error's text, and that text is the reset "+
			"step and the target's own words", err)
	}
	if d.Stage != DialStageSettings {
		t.Errorf("the sanitation failure audits as stage %q, want %q. %q sends the operator "+
			"to the network for a fault that is on the backend's session state: %s",
			d.Stage, DialStageSettings, d.Stage, d.AuditDetail())
	}
	if d.Attempts < 1 || d.Attempts > dialAttemptsPerRequest {
		t.Errorf("the sanitation failure reports %d attempts; the bound is %d, and a permit "+
			"is an instance-wide allowance other sessions are queued for",
			d.Attempts, dialAttemptsPerRequest)
	}
	if s.pinnedConn() != nil {
		t.Error("the session was left holding a backend whose state this package could not " +
			"clear, which is the inheritance the checkout proof exists to refuse")
	}
}
