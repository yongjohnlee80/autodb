package tui_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

// enrollElsewhere registers another SSH key for alice on the server and a
// device enrolled with it from ip, after this computer's last sign-in; it
// returns the key's id.
func (r *remoteRig) enrollElsewhere(t *testing.T, ip string) int64 {
	t.Helper()
	ctx := context.Background()
	_, fp := newPublicKey(t)
	keyID, err := r.srv.Store.SSHKeys.OnCtx(ctx).Set(meta.SSHKeyUserID, int64(2)).
		Set(meta.SSHKeyPublicKey, "ssh-ed25519 AAAAelsewhere").Set(meta.SSHKeyFingerprint, fp).
		Set(meta.SSHKeyCreatedAt, int64(1)).Set(meta.SSHKeyRevokedAt, int64(0)).Insert()
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour).Unix()
	if _, err := r.srv.Store.RemoteDevices.OnCtx(ctx).Set(meta.DevSSHKeyID, keyID).Set(meta.DevUserID, int64(2)).
		Set(meta.DevPublicKey, "ssh-ed25519 AAAAdevice").Set(meta.DevFingerprint, "SHA256:elsewhere-device").
		Set(meta.DevEnrolledAt, later).Set(meta.DevEnrolledIP, ip).Set(meta.DevKeyCreatedAt, later).
		Set(meta.DevLastSeenAt, later).Set(meta.DevRevokedAt, int64(0)).Set(meta.DevRevokedBy, int64(0)).Insert(); err != nil {
		t.Fatal(err)
	}
	return keyID
}

// reconnectAsAlice disconnects, dismisses the local sign-in, and connects
// again with the passphrase once (this computer is enrolled, the host key
// pinned).
func (r *remoteRig) reconnectAsAlice(t *testing.T) {
	t.Helper()
	r.s.Keys(t, decltest.Alt('m'))
	r.s.WaitForText(t, "Disconnect")
	r.s.Keys(t, key('d'))
	r.s.WaitForText(t, "┌ sign in ")
	r.s.Keys(t, esc())
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, "")
}

// keyRevoked reports whether SSH key id is revoked on the server.
func (r *remoteRig) keyRevoked(t *testing.T, id int64) bool {
	t.Helper()
	k, err := r.srv.Store.SSHKeys.OnCtx(context.Background()).With(meta.SSHKeyID, id).Get()
	if err != nil {
		t.Fatal(err)
	}
	return k.RevokedAt != 0
}

// A device enrolled elsewhere since this computer last signed in is told at
// its next sign-in; Revoke it revokes that device's SSH key, and the device
// with it.
func TestTheNextSignInTellsOfANewDeviceAndRevokesIt(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	keyID := r.enrollElsewhere(t, "198.51.100.23")
	r.reconnectAsAlice(t)
	r.s.WaitFor(t, "the notice", func(sc string) bool {
		return strings.Contains(sc, "New device enrolled from 198.51.100.23") && strings.Contains(sc, "not you? Revoke it")
	})
	r.s.Keys(t, key('r'))
	r.s.WaitFor(t, "revoked", func(string) bool {
		return strings.Contains(r.h.SourceText("App.status"), "revoked the device enrolled from 198.51.100.23")
	})
	if !r.keyRevoked(t, keyID) {
		t.Fatal("the new device's SSH key is not revoked")
	}
	n, err := r.srv.Store.RemoteDevices.OnCtx(context.Background()).With(meta.DevSSHKeyID, keyID).
		With(meta.DevRevokedAt, int64(0)).Count()
	if err != nil || n != 0 {
		t.Fatalf("the new device is still live: %d %v", n, err)
	}
	if r.h.Auth() != "signed-in" {
		t.Fatalf("revoking the other device ended this session: %s", r.h.Auth())
	}
}

// It was me leaves the device and its key alone.
func TestItWasMeLeavesTheNewDeviceAlone(t *testing.T) {
	r := newRemoteRig(t)
	r.connectAsAlice(t)
	keyID := r.enrollElsewhere(t, "198.51.100.24")
	r.reconnectAsAlice(t)
	r.s.WaitForText(t, "New device enrolled from 198.51.100.24")
	r.s.Keys(t, key('i'))
	r.s.WaitFor(t, "the notice closed", func(sc string) bool {
		return !strings.Contains(sc, "New device enrolled") && r.h.Auth() == "signed-in"
	})
	if r.keyRevoked(t, keyID) {
		t.Fatal("It was me revoked the key")
	}
}

// A first connect enrolls this computer and is told nothing, whatever
// devices the user has.
func TestAnEnrollmentIsToldNothing(t *testing.T) {
	r := newRemoteRig(t)
	r.enrollElsewhere(t, "198.51.100.25")
	r.connectAsAlice(t) // a notice opens as the sign-in takes effect, before this returns
	if sc := r.s.String(); strings.Contains(sc, "New device enrolled") {
		t.Fatalf("an enrollment was told of another device:\n%s", sc)
	}
}
