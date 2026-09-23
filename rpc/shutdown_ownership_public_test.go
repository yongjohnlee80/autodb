package rpc_test

// SHUTDOWN OWNERSHIP, PROVED AT THE VERB RATHER THAN AT THE REGISTRY.
//
// THE OWNERSHIP CELLS THAT CAME WITH THE FIX EXERCISE core/exec's registry AND
// NEVER ENTER sys.shutdown. Review found what that leaves uncovered by deleting
// the handler's entire `owner == 0` refusal: `go test ./core/exec ./rpc` stayed
// green, while over the wire a second decision could again audit and commit a
// shutdown on top of a decision that still owned admission. The registry was
// correct and the public guard in front of it was unmeasured.
//
// So these two drive the real verb over the real transport, with the first
// decision held inside its ownership window by an interceptor around the audit.
// The window is a few microseconds wide; a racing goroutine that only sometimes
// lands in it would report a missing guard as a working one.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

// auditedShutdowns counts the privileged effect: one row per decision that got
// past the guard, whatever it then did.
func auditedShutdowns(t *testing.T, f *fixture) uint64 {
	t.Helper()
	n, err := f.store.Audit.OnCtx(t.Context()).
		With(meta.AuditAction, "server_shutdown").Count()
	if err != nil {
		t.Fatalf("counting server_shutdown audit rows: %v", err)
	}
	return n
}

// A SECOND DECISION ARRIVING INSIDE THE FIRST'S WINDOW IS REFUSED AT THE VERB,
// AND HAS NO EFFECT.
//
// COMMITTING ON TOP OF A LIVE DECISION IS NOT HARMLESS. If the owner then
// abandons its shutdown it reopens transaction admission, and a server stopped
// by the second one afterwards tears down a transaction admitted in between —
// the very loss the refusal exists to prevent, reached the long way round.
func TestShutdownDecision_ADecisionArrivingInsideAnothersWindowIsRefusedAtTheVerb(t *testing.T) {
	f := newFixture(t)

	// Buffered: under the defect BOTH decisions reach the gate, and a send that
	// blocked would turn a failing cell into a hanging one.
	reached := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	rpc.SetShutdownAuditGate(f.srv, func(next func() error) error {
		reached <- struct{}{}
		<-release
		return next()
	})

	owner, loser := f.session(t), f.session(t)

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		// Deliberately unasserted here: a t.Fatal off the test goroutine is not
		// a failure this package can report. The owner's outcome is asserted
		// below, on the audit trail and on the effect.
		owner.call("sys.shutdown", f.rootTok)
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the first decision never reached its audit; the cell cannot hold a " +
			"window it never entered")
	}

	// THE WATCHDOG NAMES THE DEFECT. Without the guard the loser parks in the
	// gate too and the call below blocks until the client's own read deadline,
	// which would fail this cell with "decode: i/o timeout" and send the next
	// reader to the transport.
	settled := make(chan struct{})
	go func() {
		select {
		case <-settled:
		case <-time.After(3 * time.Second):
			t.Errorf("a second sys.shutdown did not return while another decision " +
				"owned admission — it reached the audit instead of being refused in " +
				"front of it, which is the privileged effect this guard exists to stop")
		}
	}()
	errVal, result := loser.call("sys.shutdown", f.rootTok)
	close(settled)

	if errVal == nil {
		t.Fatalf("a second decision was ADMITTED while another owned admission, and "+
			"answered %#v", result)
	}
	m, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("refusal shape: %#v", errVal)
	}
	if got, _ := m["code"].(int64); got != int64(rpc.CodeShutdownBlocked) {
		t.Errorf("refusal code = %v, want CodeShutdownBlocked (%d)", m["code"],
			rpc.CodeShutdownBlocked)
	}
	// THE REFUSAL MUST NAME THE RIGHT CAUSE. CodeShutdownBlocked is also what an
	// open transaction earns, and an operator told to commit their work when
	// what actually happened was a concurrent decision is sent to look in the
	// wrong place entirely.
	if msg, _ := m["message"].(string); msg == "" ||
		!containsAll(msg, "another shutdown decision", "Retry") {
		t.Errorf("refusal message = %q; it must say a concurrent decision holds the "+
			"server, not that a transaction is open", m["message"])
	}

	// THE EFFECT ITSELF: the refused decision wrote no audit row, because it
	// never got as far as one. The owner has not written its own yet — it is
	// still parked in front of the write — so this reads exactly zero.
	if n := auditedShutdowns(t, f); n != 0 {
		t.Errorf("%d server_shutdown audit rows while the only admitted decision is "+
			"still parked in front of its own write; a refused decision reached the "+
			"privileged effect", n)
	}

	releaseAll()
	<-ownerDone

	// AND THE CONTROL, WITHOUT WHICH EVERY ASSERTION ABOVE PASSES ON A VERB
	// THAT REFUSES EVERYONE. The owner's decision really did go through.
	if n := auditedShutdowns(t, f); n != 1 {
		t.Errorf("%d server_shutdown audit rows after the owner completed, want 1", n)
	}
}

// AN OWNER WHOSE AUDIT FAILS HANDS ADMISSION BACK, AT THE VERB.
//
// THE OTHER HALF, AND IT IS NOT SYMMETRICAL. A decision that closes admission
// and then does not stop the server must reopen it, or the daemon spends the
// rest of its life refusing to begin transactions it is never going to end —
// and the refusal above would be permanent rather than momentary. Asserting it
// through the verb, on a fresh decision that must be ADMITTED, is what
// distinguishes "handed back" from "never taken".
func TestShutdownDecision_AnOwnerWhoseAuditFailsLeavesTheDoorOpenForTheNext(t *testing.T) {
	f := newFixture(t)

	var attempts int
	var mu sync.Mutex
	rpc.SetShutdownAuditGate(f.srv, func(next func() error) error {
		mu.Lock()
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			// The write never happens, so the handler takes the failure path
			// that aborts — its own, not a copy of it living in this hook.
			return errors.New("the audit sink is unavailable")
		}
		return next()
	})

	c := f.session(t)

	if errVal, _ := c.call("sys.shutdown", f.rootTok); errVal == nil {
		t.Fatal("a shutdown whose audit failed reported success; an unaudited " +
			"privileged effect must never happen")
	}
	if n := auditedShutdowns(t, f); n != 0 {
		t.Fatalf("%d server_shutdown audit rows after a failed audit, want 0", n)
	}

	// THE ASSERTION: the next decision is admitted, which it can only be if the
	// first one handed ownership back.
	errVal, result := c.call("sys.shutdown", f.rootTok)
	if errVal != nil {
		m, _ := errVal.(map[string]any)
		t.Fatalf("the next decision was refused %#v — the aborted one never handed "+
			"admission back, so this daemon now refuses every transaction it is "+
			"asked to begin and every shutdown it is asked to perform", m)
	}
	if m, _ := result.(map[string]any); m == nil || m["stopping"] != true {
		t.Errorf("the admitted decision answered %#v, want stopping:true", result)
	}
	if n := auditedShutdowns(t, f); n != 1 {
		t.Errorf("%d server_shutdown audit rows after the second decision, want 1", n)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
