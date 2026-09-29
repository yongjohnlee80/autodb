package auth

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// seedSession inserts a session row directly: the remote login that mints a
// device-bound one does not exist yet, and the reconciliation must not
// depend on it.
func seedSession(t *testing.T, store *meta.Store, userID, deviceID, revoked int64, hash string) int64 {
	t.Helper()
	id, err := store.Sessions.OnCtx(context.Background()).
		Set(meta.SessTokenHash, []byte(hash)).Set(meta.SessUserID, userID).
		Set(meta.SessIP, "203.0.113.7").Set(meta.SessCreatedAt, int64(1)).
		Set(meta.SessExpiresAt, int64(9_999_999_999)).Set(meta.SessRevoked, revoked).
		Set(meta.SessDeviceID, deviceID).Set(meta.SessAttachedConn, "c0ffee").
		Insert()
	if err != nil {
		t.Fatalf("seeding a session: %v", err)
	}
	return id
}

func revokedOf(t *testing.T, store *meta.Store, id int64) int64 {
	t.Helper()
	s, err := store.Sessions.OnCtx(context.Background()).With(meta.SessID, id).Get()
	if err != nil {
		t.Fatalf("reading session %d: %v", id, err)
	}
	return s.Revoked
}

// Every live device-bound session is revoked; a local session and one already
// revoked are left as they were, and the count says how many changed.
func TestRevokeRemoteSessionsRevokesOnlyLiveDeviceBoundOnes(t *testing.T) {
	s, store, _ := newSvc(t)
	_, root := mustBootstrap(t, s)
	local := seedSession(t, store, root.userID, 0, 0, "local")
	remote := seedSession(t, store, root.userID, 7, 0, "remote")
	remote2 := seedSession(t, store, root.userID, 8, 0, "remote2")
	gone := seedSession(t, store, root.userID, 9, 1, "already-revoked")

	n, err := s.RevokeRemoteSessions(context.Background())
	if err != nil {
		t.Fatalf("RevokeRemoteSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d sessions, want 2", n)
	}
	for id, want := range map[int64]int64{local: 0, remote: 1, remote2: 1, gone: 1} {
		if got := revokedOf(t, store, id); got != want {
			t.Errorf("session %d: revoked=%d, want %d", id, got, want)
		}
	}
	// Twice is the same as once: nothing is left to revoke.
	if n, err := s.RevokeRemoteSessions(context.Background()); err != nil || n != 0 {
		t.Errorf("second run: %d, %v; want 0, nil", n, err)
	}
}
