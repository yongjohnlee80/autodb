package exec

import "testing"

// WOULD STOPPING NOW THROW AWAY WORK SOMEBODY HAS NOT FINISHED?
//
// The count behind the shutdown refusal. It asks each session for its own
// transaction phase, which is what a CALLER's transaction state actually is.
//
// Deliberately NOT inTransactionHoldingBackend, its neighbour: that one asks
// whether a physical backend is pinned, which answers a CAPACITY question
// ("is this slot reclaimable"). A session can be inside a transaction without
// the engine having pinned a backend for it, and losing its work is no less a
// loss for that -- so counting by pinned backend would under-report exactly
// the sessions this refusal exists to protect.
func TestCountInTransaction(t *testing.T) {
	t.Parallel()

	r := newSessionRegistry(64, 64)

	// POSITIVE CONTROL FIRST: an empty registry must not block a shutdown, or
	// a counter that returned a constant would satisfy every case below while
	// making the server impossible to stop.
	if n := r.countInTransaction(); n != 0 {
		t.Fatalf("an empty registry counted %d open transactions", n)
	}

	idle := &session{id: "idle", userID: 1, connID: 1}
	inTx := &session{id: "in-tx", userID: 1, connID: 1}
	inTx.txPhase = txActive
	// AN ABORTED TRANSACTION COUNTS TOO, deliberately. It holds no work worth
	// saving, but it is still a transaction the client has to end, and the
	// canonical predicate -- session.inTransaction() -- is txPhase != txNone.
	// A second, narrower notion of "in transaction" is exactly the divergence
	// this file already carries once (s.tx != nil vs txPhase) and does not
	// need twice. Refusing is self-healing: the idle-in-transaction timeout
	// reaps it.
	alsoInTx := &session{id: "also", userID: 2, connID: 1}
	alsoInTx.txPhase = txAborted

	r.mu.Lock()
	r.byID[idle.id] = idle
	r.byID[inTx.id] = inTx
	r.byID[alsoInTx.id] = alsoInTx
	r.mu.Unlock()

	// An idle session is NOT a blocker: it holds nothing a stop would destroy,
	// and counting it would make the server unstoppable whenever anyone had a
	// session open at all.
	if n := r.countInTransaction(); n != 2 {
		t.Errorf("counted %d, want 2: one idle, one active and one aborted", n)
	}

	// And a transaction that ends stops blocking.
	inTx.mu.Lock()
	inTx.txPhase = txNone
	inTx.mu.Unlock()
	if n := r.countInTransaction(); n != 1 {
		t.Errorf("counted %d after one transaction ended, want 1", n)
	}
}
