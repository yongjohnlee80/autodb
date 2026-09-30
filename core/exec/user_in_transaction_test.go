package exec

import "testing"

// UserInTransaction sees one user's open (or aborted, still unended)
// transaction, and not another user's, nor an idle session.
func TestUserInTransactionSeesOnlyThatUsersTransaction(t *testing.T) {
	t.Parallel()
	e := &Engine{sessions: newSessionRegistry(64, 64)}
	idle := &session{id: "idle", userID: 1, connID: 1}
	other := &session{id: "other", userID: 2, connID: 1}
	other.txPhase = txActive
	e.sessions.mu.Lock()
	e.sessions.byID[idle.id] = idle
	e.sessions.byID[other.id] = other
	e.sessions.mu.Unlock()
	if e.UserInTransaction(1) {
		t.Fatal("user 1 has only an idle session, and another user's transaction was counted for them")
	}
	if !e.UserInTransaction(2) {
		t.Fatal("user 2's open transaction was not seen")
	}
	idle.mu.Lock()
	idle.txPhase = txAborted
	idle.mu.Unlock()
	if !e.UserInTransaction(1) {
		t.Fatal("an aborted transaction the client has not ended was not seen")
	}
}
