package meta

import (
	"context"
	"testing"
)

// remoteStores is a store at the latest schema on each engine this run has,
// with one user to own keys.
func remoteStores(t *testing.T) map[string]*Store {
	t.Helper()
	out := map[string]*Store{}
	for name, cfgOf := range scriptEngines(t) {
		s := open(t, cfgOf(t))
		if _, err := s.Users.OnCtx(context.Background()).
			Set(UserName, "alice").Set(UserPassHash, []byte("x")).Set(UserRole, RoleReader).
			Set(UserCreatedAt, int64(1)).Set(UserUpdatedAt, int64(1)).Insert(); err != nil {
			t.Fatalf("%s: seeding a user: %v", name, err)
		}
		out[name] = s
	}
	return out
}

func addKey(s *Store, fp string, revokedAt int64) (int64, error) {
	return s.SSHKeys.OnCtx(context.Background()).
		Set(SSHKeyUserID, int64(1)).Set(SSHKeyPublicKey, "ssh-ed25519 AAAA"+fp).
		Set(SSHKeyFingerprint, fp).Set(SSHKeyCreatedAt, int64(1)).
		Set(SSHKeyRevokedAt, revokedAt).Insert()
}

func addDevice(s *Store, keyID int64, fp string, revokedAt int64) (int64, error) {
	return s.RemoteDevices.OnCtx(context.Background()).
		Set(DevSSHKeyID, keyID).Set(DevUserID, int64(1)).
		Set(DevPublicKey, "ed25519:"+fp).Set(DevFingerprint, fp).
		Set(DevEnrolledAt, int64(1)).Set(DevEnrolledIP, "203.0.113.7").
		Set(DevKeyCreatedAt, int64(1)).Set(DevRevokedAt, revokedAt).Insert()
}

// A fingerprint names one LIVE key: a second live key with it is refused, and
// once the first is revoked the same fingerprint can be registered again.
func TestAFingerprintNamesOneLiveSSHKey(t *testing.T) {
	for name, s := range remoteStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := addKey(s, "SHA256:aaa", 0); err != nil {
				t.Fatalf("first key: %v", err)
			}
			if _, err := addKey(s, "SHA256:aaa", 0); err == nil {
				t.Fatal("a second live key with the same fingerprint was accepted")
			}
			if err := s.SSHKeys.OnCtx(context.Background()).With(SSHKeyFingerprint, "SHA256:aaa").
				Set(SSHKeyRevokedAt, int64(5)).Update(); err != nil {
				t.Fatalf("revoking: %v", err)
			}
			if _, err := addKey(s, "SHA256:aaa", 0); err != nil {
				t.Fatalf("re-registering after a revoke: %v", err)
			}
		})
	}
}

// One live device per SSH key: the store itself decides a race between two
// machines enrolling the same fresh key, and a revoked device
// frees the key for the next.
func TestAnSSHKeyHasOneLiveDevice(t *testing.T) {
	for name, s := range remoteStores(t) {
		t.Run(name, func(t *testing.T) {
			key, err := addKey(s, "SHA256:bbb", 0)
			if err != nil {
				t.Fatalf("key: %v", err)
			}
			first, err := addDevice(s, key, "dev-1", 0)
			if err != nil {
				t.Fatalf("first device: %v", err)
			}
			if _, err := addDevice(s, key, "dev-2", 0); err == nil {
				t.Fatal("a second live device for the same key was accepted")
			}
			if err := s.RemoteDevices.OnCtx(context.Background()).With(DevID, first).
				Set(DevRevokedAt, int64(5)).Update(); err != nil {
				t.Fatalf("revoking: %v", err)
			}
			if _, err := addDevice(s, key, "dev-2", 0); err != nil {
				t.Fatalf("enrolling after a revoke: %v", err)
			}
		})
	}
}

// A session written without the remote columns, as every local login and
// every older binary writes one, reads back as local: device 0, owned by no
// connection, not detached.
func TestASessionWithoutTheRemoteColumnsIsLocal(t *testing.T) {
	for name, s := range remoteStores(t) {
		t.Run(name, func(t *testing.T) {
			id, err := s.Sessions.OnCtx(context.Background()).
				Set(SessTokenHash, []byte("h")).Set(SessUserID, int64(1)).Set(SessIP, "local").
				Set(SessCreatedAt, int64(1)).Set(SessExpiresAt, int64(2)).Insert()
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			got, err := s.Sessions.OnCtx(context.Background()).With(SessID, id).Get()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.DeviceID != 0 || got.AttachedConn != "" || got.DetachedUntil != 0 {
				t.Fatalf("defaults: device %d, conn %q, detached %d; want 0, \"\", 0", got.DeviceID, got.AttachedConn, got.DetachedUntil)
			}
		})
	}
}
