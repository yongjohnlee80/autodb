package exec

import "testing"

// WHAT IS ACTUALLY RUNNING RIGHT NOW?
//
// The figure behind the restart confirmation, which tells an operator how much
// work a restart is about to cancel. Wrong in either direction is a lie to
// somebody deciding whether to take a production surface down.
//
// Deliberately NOT countInTransaction, its neighbour: a session parked between
// statements inside a transaction has nothing in flight, and a session running
// a statement outside one has work in flight and no transaction. They are two
// populations and naming either as the other is untrue.
//
// And deliberately not the raw busy bit either -- see the teardown case.
func TestCountExecuting(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(64, 64)

	// POSITIVE CONTROL FIRST: an empty registry counts nothing, or a counter
	// returning a constant would satisfy every case below.
	if n := r.countExecuting(); n != 0 {
		t.Fatalf("an empty registry counted %d statements in flight", n)
	}

	idle := &session{id: "idle", userID: 1, connID: 1}

	// Parked INSIDE a transaction: the transaction is open, and nothing is
	// running. This is the case that makes the two counters different, and the
	// one that would be miscounted by reusing countInTransaction here.
	parked := &session{id: "parked", userID: 1, connID: 1}
	parked.txPhase = txActive

	running := &session{id: "running", userID: 2, connID: 1}
	running.busy = true

	// A TEARDOWN RESERVATION IS NOT A STATEMENT. claimTeardown takes the same
	// one-slot gate busy guards, so the raw bit cannot tell them apart -- and
	// counting it inflated the figure the operator is shown, promising to
	// cancel work that does not exist. tearingDown is what distinguishes them,
	// which is the job its own comment gives it.
	teardown := &session{id: "teardown", userID: 2, connID: 1}
	teardown.busy = true
	teardown.tearingDown = true

	// Both at once: a statement running inside an open transaction is ONE
	// statement in flight and ONE open transaction, counted by each.
	both := &session{id: "both", userID: 3, connID: 1}
	both.busy = true
	both.txPhase = txActive

	r.mu.Lock()
	for _, s := range []*session{idle, parked, running, teardown, both} {
		r.byID[s.id] = s
	}
	r.mu.Unlock()

	if n := r.countExecuting(); n != 2 {
		t.Errorf("counted %d statements in flight, want 2 (running, both): idle, a parked "+
			"transaction and a teardown reservation are not statements", n)
	}
	// The other counter over the same registry, so the two cannot quietly
	// collapse into one another.
	if n := r.countInTransaction(); n != 2 {
		t.Errorf("counted %d open transactions, want 2 (parked, both)", n)
	}

	// TRANSITIONS, because a count that is only ever read once is a count that
	// has never been shown to follow anything.
	running.mu.Lock()
	running.busy = false
	running.mu.Unlock()
	if n := r.countExecuting(); n != 1 {
		t.Errorf("counted %d after a statement finished, want 1", n)
	}

	// A teardown that gives its slot back is still not a statement.
	teardown.mu.Lock()
	teardown.busy = false
	teardown.tearingDown = false
	teardown.mu.Unlock()
	if n := r.countExecuting(); n != 1 {
		t.Errorf("counted %d after a teardown released its slot, want 1", n)
	}

	// And the idle session starts one.
	idle.mu.Lock()
	idle.busy = true
	idle.mu.Unlock()
	if n := r.countExecuting(); n != 2 {
		t.Errorf("counted %d after an idle session began a statement, want 2", n)
	}
}
