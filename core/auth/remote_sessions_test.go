package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
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

func seedKey(t *testing.T, store *meta.Store, userID int64, fp string, revokedAt int64) int64 {
	t.Helper()
	id, err := store.SSHKeys.OnCtx(context.Background()).
		Set(meta.SSHKeyUserID, userID).Set(meta.SSHKeyPublicKey, "ssh-ed25519 AAAA"+fp).
		Set(meta.SSHKeyFingerprint, fp).Set(meta.SSHKeyCreatedAt, int64(1)).
		Set(meta.SSHKeyRevokedAt, revokedAt).Insert()
	if err != nil {
		t.Fatalf("seeding a key: %v", err)
	}
	return id
}

// A live key of an enabled user resolves to its id and owner; an unknown, a
// revoked, and a disabled user's key are all the one ErrUnknownKey.
func TestRemoteKeyOwnerAnswersOnlyForALiveKeyOfAnEnabledUser(t *testing.T) {
	s, store, _ := newSvc(t)
	_, root := mustBootstrap(t, s)
	live := seedKey(t, store, root.userID, "SHA256:live", 0)
	seedKey(t, store, root.userID, "SHA256:revoked", 9)
	ctx := context.Background()
	if k, u, err := s.RemoteKeyOwner(ctx, "SHA256:live"); err != nil || k != live || u != root.userID {
		t.Fatalf("a live key: %d, %d, %v; want %d, %d", k, u, err, live, root.userID)
	}
	for _, fp := range []string{"SHA256:nobody", "SHA256:revoked"} {
		if _, _, err := s.RemoteKeyOwner(ctx, fp); !errors.Is(err, remote.ErrUnknownKey) {
			t.Errorf("%s: %v; want ErrUnknownKey", fp, err)
		}
	}
	if err := store.Users.OnCtx(ctx).With(meta.UserID, root.userID).Set(meta.UserDisabled, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RemoteKeyOwner(ctx, "SHA256:live"); !errors.Is(err, remote.ErrUnknownKey) {
		t.Errorf("a disabled user's key: %v; want ErrUnknownKey", err)
	}
}
