package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// pubText is a fresh ed25519 key in authorized-key form.
func pubText(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(pub)
	return string(ssh.MarshalAuthorizedKey(k))
}

func rsaText(t *testing.T, bits int) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(&priv.PublicKey)
	return string(ssh.MarshalAuthorizedKey(k))
}

// withReader is a service with root (admin) and alice (reader), and their
// tokens.
func withReader(t *testing.T) (*Service, *meta.Store, string, string, int64) {
	t.Helper()
	s, store, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	id, err := s.CreateUser(ctx, rootTok, "alice", "alice-passphrase-long", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	aliceTok, _, err := s.Login(ctx, "alice", "alice-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	return s, store, rootTok, aliceTok, id
}

func keyCount(t *testing.T, store *meta.Store) uint64 {
	t.Helper()
	n, err := store.SSHKeys.OnCtx(context.Background()).Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Adding a key to one's own profile needs one's passphrase; an admin adds to
// another's without it; a user cannot add to someone else's. A key names one
// account: the same key twice is refused, on any profile.
func TestAddingAnSSHKey(t *testing.T) {
	s, store, rootTok, aliceTok, aliceID := withReader(t)
	ctx := context.Background()

	if _, err := s.AddSSHKey(ctx, aliceTok, 0, pubText(t), "laptop", "wrong", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("self-add with a wrong passphrase: %v; want ErrBadCredentials", err)
	}
	if n := keyCount(t, store); n != 0 {
		t.Fatalf("keys %d after a refused self-add", n)
	}
	key := pubText(t)
	k, err := s.AddSSHKey(ctx, aliceTok, 0, key, " laptop ", "alice-passphrase-long", testIP)
	if err != nil || k.UserID != aliceID || k.Label != "laptop" || k.Device != nil {
		t.Fatalf("self-add: %+v, %v", k, err)
	}
	if _, err := s.AddSSHKey(ctx, rootTok, aliceID, pubText(t), "desk", "", testIP); err != nil {
		t.Fatalf("an admin adding to another's profile: %v", err)
	}
	if _, err := s.AddSSHKey(ctx, rootTok, 0, pubText(t), "own", "", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("an admin adding to their OWN profile without the passphrase: %v; want ErrBadCredentials", err)
	}
	if _, err := s.AddSSHKey(ctx, aliceTok, 1, pubText(t), "sneaky", "alice-passphrase-long", testIP); !errors.Is(err, ErrDenied) {
		t.Fatalf("a reader adding to root's profile: %v; want ErrDenied", err)
	}
	if _, err := s.AddSSHKey(ctx, rootTok, 1, key, "copy", rootPass, testIP); !errors.Is(err, ErrSSHKeyTaken) {
		t.Fatalf("alice's key on root's profile: %v; want ErrSSHKeyTaken", err)
	}
	if n := auditCount(t, store, "user_ssh_key_added"); n != 2 {
		t.Fatalf("user_ssh_key_added rows %d, want 2", n)
	}
}

// Only one plain key is read: garbage, options, DSA-era sizes and short RSA
// are refused; RSA 3072 and ed25519 are accepted.
func TestParseSSHKey(t *testing.T) {
	ok := pubText(t)
	for name, text := range map[string]string{
		"garbage":      "not a key",
		"with options": `from="10.0.0.1" ` + ok,
		"two keys":     strings.TrimSpace(ok) + "\n" + pubText(t),
		"rsa 2048":     rsaText(t, 2048),
	} {
		if _, _, _, err := ParseSSHKey(text); !errors.Is(err, ErrSSHKeyInvalid) {
			t.Errorf("%s: %v; want ErrSSHKeyInvalid", name, err)
		}
	}
	for name, text := range map[string]string{"ed25519": ok, "rsa 3072": rsaText(t, 3072)} {
		if _, fp, _, err := ParseSSHKey(text); err != nil || !strings.HasPrefix(fp, "SHA256:") {
			t.Errorf("%s: %q, %v", name, fp, err)
		}
	}
}

// Keys and devices are their owner's to list and revoke, and an admin's; a
// user sees another's as absent.
func TestKeysAndDevicesAreTheOwnersOrAnAdmins(t *testing.T) {
	s, store, rootTok, aliceTok, aliceID := withReader(t)
	ctx := context.Background()
	rootKey := seedRemoteToken(t, store, 1, "r", "conn-r")
	aliceKey := seedRemoteToken(t, store, aliceID, "a", "conn-a")

	mine, err := s.ListSSHKeys(ctx, aliceTok, 0)
	if err != nil || len(mine) != 1 || mine[0].ID != aliceKey.sshKey || mine[0].Device == nil || mine[0].Device.ID != aliceKey.device {
		t.Fatalf("alice's keys: %+v, %v", mine, err)
	}
	if _, err := s.ListSSHKeys(ctx, aliceTok, -1); !errors.Is(err, ErrDenied) {
		t.Fatalf("a reader listing everyone's keys: %v; want ErrDenied", err)
	}
	if _, err := s.ListSSHKeys(ctx, aliceTok, 1); !errors.Is(err, ErrDenied) {
		t.Fatalf("a reader listing root's keys: %v; want ErrDenied", err)
	}
	if all, err := s.ListSSHKeys(ctx, rootTok, -1); err != nil || len(all) != 2 {
		t.Fatalf("an admin listing everyone's keys: %d, %v", len(all), err)
	}
	if err := s.RevokeSSHKey(ctx, aliceTok, rootKey.sshKey, testIP); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reader revoking root's key: %v; want ErrNotFound", err)
	}
	if err := s.RevokeDevice(ctx, aliceTok, rootKey.device, testIP); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reader revoking root's device: %v; want ErrNotFound", err)
	}
	if err := s.LabelSSHKey(ctx, aliceTok, rootKey.sshKey, "x", testIP); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reader labelling root's key: %v; want ErrNotFound", err)
	}
	if err := s.LabelSSHKey(ctx, aliceTok, aliceKey.sshKey, "home", testIP); err != nil {
		t.Fatalf("labelling one's own key: %v", err)
	}
	if err := s.RevokeDevice(ctx, aliceTok, aliceKey.device, testIP); err != nil {
		t.Fatalf("revoking one's own device: %v", err)
	}
	if devs, err := s.ListDevices(ctx, aliceTok, 0, true); err != nil || len(devs) != 1 || devs[0].RevokedAt == 0 {
		t.Fatalf("alice's devices with revoked: %+v, %v", devs, err)
	}
	if devs, err := s.ListDevices(ctx, aliceTok, 0, false); err != nil || len(devs) != 0 {
		t.Fatalf("alice's live devices: %+v, %v", devs, err)
	}
	if err := s.RevokeSSHKey(ctx, rootTok, aliceKey.sshKey, testIP); err != nil {
		t.Fatalf("an admin revoking alice's key: %v", err)
	}
}

// Revoking a key revokes its device and every session bound to it, in one
// transaction; revoking a device, its sessions. Sessions of other devices
// are untouched.
func TestRevocationTakesTheSessionsWithIt(t *testing.T) {
	s, store, rootTok, _, _ := withReader(t)
	ctx := context.Background()
	a := seedRemoteToken(t, store, 1, "a", "conn-a")
	b := seedRemoteToken(t, store, 1, "b", "conn-b")
	c := seedRemoteToken(t, store, 1, "c", "conn-c")
	if err := s.RevokeSSHKey(ctx, rootTok, a.sshKey, testIP); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(ctx, rootTok, b.device, testIP); err != nil {
		t.Fatal(err)
	}
	revoked := func(rt remoteToken) bool {
		sess, err := store.Sessions.OnCtx(ctx).With(meta.SessID, rt.session).Get()
		if err != nil {
			t.Fatal(err)
		}
		return sess.Revoked != 0
	}
	dev, _ := store.RemoteDevices.OnCtx(ctx).With(meta.DevID, a.device).Get()
	if dev.RevokedAt == 0 || !revoked(a) || !revoked(b) || revoked(c) {
		t.Fatalf("key revoke: device revoked %d, sessions a=%v b=%v c=%v; want a and b revoked, c live",
			dev.RevokedAt, revoked(a), revoked(b), revoked(c))
	}
	if n := auditCount(t, store, "user_ssh_key_revoked") + auditCount(t, store, "user_remote_device_revoked"); n != 2 {
		t.Fatalf("revocation audit rows %d, want 2", n)
	}
}

// The blocked addresses are an admin's, each with its latest refusal's
// reason.
func TestListBlocksIsAnAdmins(t *testing.T) {
	s, store, rootTok, aliceTok, _ := withReader(t)
	ctx := context.Background()
	if err := store.RemoteIPBlocks.OnCtx(ctx).Set(meta.BlockPrefix, "198.51.100.7").
		Set(meta.BlockFailures, int64(3)).Set(meta.BlockLastFailureAt, int64(10)).
		Set(meta.BlockUntil, int64(9_999_999_999)).Upsert(); err != nil {
		t.Fatal(err)
	}
	for i, reason := range []string{"login_failed", "device_mismatch"} {
		if _, err := store.RemoteDenials.OnCtx(ctx).Set(meta.DenialEventID, string(rune('a'+i))).
			Set(meta.DenialPrefix, "198.51.100.7").Set(meta.DenialOccurredAt, int64(5+i)).
			Set(meta.DenialReason, reason).Insert(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ListBlocks(ctx, aliceTok); !errors.Is(err, ErrDenied) {
		t.Fatalf("a reader listing blocks: %v; want ErrDenied", err)
	}
	bs, err := s.ListBlocks(ctx, rootTok)
	if err != nil || len(bs) != 1 || bs[0].Reason != "device_mismatch" || bs[0].Failures != 3 {
		t.Fatalf("blocks %+v, %v; want one, reason device_mismatch", bs, err)
	}
}
