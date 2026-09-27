package frontdoor

import (
	"sync"
	"testing"
	"time"
)

// The engine's wire gate at accept: while it is closed a connection is refused
// as server-stopping; a slot it granted is given back when this admitter then
// refuses, and when the connection's ticket is released — so the engine counts
// exactly the connections this front door let in.

type fakeGate struct {
	mu     sync.Mutex
	closed bool
	held   int
}

func (g *fakeGate) AdmitWireConnection() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, false
	}
	g.held++
	return func() { g.mu.Lock(); g.held--; g.mu.Unlock() }, true
}

func (g *fakeGate) count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.held }

func TestAClosedWireGateRefusesAtAcceptAsServerStopping(t *testing.T) {
	a := newAdmitter(10, 10, 5, 1<<20, time.Now)
	g := &fakeGate{closed: true}
	a.gate = g
	tkt, reason := a.admit("10.0.0.1:5000")
	if tkt != nil || reason != reasonServerStopping {
		t.Fatalf("admit with the gate closed: ticket %v reason %q, want nil and %q", tkt, reason, reasonServerStopping)
	}
	if a.conns != 0 {
		t.Errorf("a refused connection reserved %d slot(s) in the admitter", a.conns)
	}
}

func TestTheWireGatesSlotComesBackOnACapRefusalAndOnRelease(t *testing.T) {
	g := &fakeGate{}
	full := newAdmitter(0, 10, 5, 1<<20, time.Now) // no capacity: every admit refuses
	full.gate = g
	if tkt, _ := full.admit("10.0.0.1:5000"); tkt != nil {
		t.Fatal("a zero-capacity admitter admitted")
	}
	if n := g.count(); n != 0 {
		t.Errorf("after the admitter's own refusal the engine still counts %d connection(s)", n)
	}
	a := newAdmitter(10, 10, 5, 1<<20, time.Now)
	a.gate = g
	tkt, _ := a.admit("10.0.0.1:5000")
	if tkt == nil || g.count() != 1 {
		t.Fatalf("an admitted connection: ticket %v, engine counts %d; want a ticket and 1", tkt, g.count())
	}
	tkt.release()
	tkt.release() // idempotent
	if n := g.count(); n != 0 {
		t.Errorf("after the ticket's release the engine still counts %d connection(s)", n)
	}
}
