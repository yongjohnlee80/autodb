package exec

import "context"

// WHEN SOMEBODY NEEDS A CONNECTION AND EVERY SLOT IS TAKEN, LOOK FOR ONE THAT
// IS MERELY BEING HELD RATHER THAN USED.
//
// A wire session keeps its backend for its whole life, so a developer who
// opened a session this morning and has not typed since is holding a physical
// connection that nobody is using. Waiting behind that is the wrong answer
// when the connection can simply be handed over: the holder is idle, owns no
// transaction and owns no server-side objects, so taking the backend away
// costs them nothing they can observe — their next statement takes a fresh
// one.
//
// THE COST OF GETTING THIS WRONG IS SILENT DATA LOSS, which is why the
// predicate is narrow and every condition is required rather than indicative:
//
//   - a transaction open  -> reclaiming rolls back work the caller did not
//     abandon, and they find out by their next statement failing;
//   - a request in flight -> we would be cancelling work that is within its
//     bounds, which the ruling forbids outright;
//   - objects in the store -> a prepared statement or portal lives on THAT
//     backend, so the session's next Bind meets 26000 for an object it
//     correctly believes it created.
//
// Only the fourth state — idle, quiet, empty — is safe, and it is the common
// one. This is row 3a of the reclamation ladder. Row 3b, which reclaims from a
// session that DOES hold objects by terminating it with a framed outcome, is
// deliberately not implemented here: it ends somebody's session, so it belongs
// with the bound-triggered termination work and its own review rather than
// arriving quietly inside a capacity fix.

// reclaimableIdle finds a session whose backend can be taken without the
// holder being able to tell, or nil.
//
// THE FIRST MATCH WINS AND THAT IS ENOUGH. Choosing the "best" victim — the
// longest idle, say — needs a comparison across sessions whose state is
// changing while it runs, and buys nothing: every candidate is equally
// reclaimable by construction, because the predicate already excludes anyone
// who would notice.
func (r *sessionRegistry) reclaimableIdle() *session {
	r.mu.Lock()
	sessions := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	// Judged outside the registry lock: reading a session's state takes that
	// session's own mutex, and holding both is a lock ordering this package
	// does not otherwise have.
	for _, s := range sessions {
		s.mu.Lock()
		eligible := s.pc != nil && // holds a backend at all
			s.tx == nil && //  no transaction to roll back
			!s.busy && //      no request in flight
			s.objectsEmpty() // nothing of theirs lives on that backend
		s.mu.Unlock()
		if eligible {
			return s
		}
	}
	return nil
}

// objectsEmpty reports whether the session has nothing on its backend that
// would not survive a move. Caller holds s.mu.
//
// PENDING CLOSES COUNT AS PRESENT. A Close that has been queued but not
// acknowledged means the target may still hold the object, so the store is not
// yet empty — treating it as empty would hand away a backend that still has
// the session's state on it, which is the exact failure this predicate exists
// to prevent.
func (s *session) objectsEmpty() bool {
	if s.ext == nil {
		return true
	}
	return len(s.ext.statements) == 0 &&
		len(s.ext.portals) == 0 &&
		len(s.ext.pendingCloses) == 0
}

// reclaimOneIdleBackend takes a backend from an idle holder and returns
// whether it freed one.
//
// IT GOES THROUGH THE RELEASE GATE, NOT AROUND IT. The gate is what proves the
// backend is clean before it returns to the pool, and a reclaim path that
// skipped it would be the one route by which an unproved connection reaches
// another developer — the leak the gate exists to stop, reintroduced through
// the door marked "capacity".
func (e *Engine) reclaimOneIdleBackend(ctx context.Context) bool {
	if e.sessions == nil {
		return false
	}
	s := e.sessions.reclaimableIdle()
	if s == nil {
		return false
	}

	// Re-check under the session's own lock before detaching. The candidate was
	// chosen from a snapshot, and a session that started a statement in between
	// must not have its backend taken mid-flight.
	s.mu.Lock()
	if s.pc == nil || s.tx != nil || s.busy || !s.objectsEmpty() {
		s.mu.Unlock()
		return false
	}
	pc := s.pc
	s.pc = nil
	s.mu.Unlock()

	v := e.releaseBackend(ctx, s, pc, nil)
	if !v.pooled {
		// The gate could not prove it clean and has already destroyed it. The
		// slot is still freed, which is what the caller asked about; the
		// backend simply does not go back to the pool.
		e.logf("connection %d: an idle backend reclaimed for capacity could not be "+
			"proved clean and was destroyed (%s)", s.connID, v.reason())
	}
	return true
}
