package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/rpc"
)

// addKey registers a new SSH key for userID and returns it.
func (r *remoteRig) addKey(t *testing.T, userID int64) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewSignerFromKey(priv)
	if _, err := r.store.SSHKeys.OnCtx(t.Context()).Set(meta.SSHKeyUserID, userID).
		Set(meta.SSHKeyPublicKey, string(ssh.MarshalAuthorizedKey(key.PublicKey()))).
		Set(meta.SSHKeyFingerprint, ssh.FingerprintSHA256(key.PublicKey())).
		Set(meta.SSHKeyCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	return key
}

// signedIn is a remote connection signed in with dev, and its token.
func (r *remoteRig) signedIn(t *testing.T, addr net.Addr, dev device) (*remoteClient, string) {
	t.Helper()
	c := r.connect(t, addr, r.key, "")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	tok, err := c.login("root", "root-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

// sessionOf is the session row of token.
func (r *remoteRig) sessionOf(t *testing.T, token string) *meta.Session {
	t.Helper()
	s, err := r.store.Sessions.OnCtx(t.Context()).With(meta.SessTokenHash, tokenHashOf(token)).Get()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// resumer is a new connection of dev, proved, ready to resume.
func (r *remoteRig) resumer(t *testing.T, addr net.Addr, dev device) *remoteClient {
	t.Helper()
	c := r.connect(t, addr, r.key, "")
	if at, err := c.attest(dev); err != nil || at["enrolled"] != true {
		t.Fatalf("proving the enrolled device: %v, %#v", err, at)
	}
	return c
}

// Within the grace, a new connection of the same device takes the session up
// with only its token: an authenticated call works on it, and it keeps the
// daemon from an idle restart.
func TestAResumeWithinTheGraceNeedsNoPassphrase(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	old, tok := r.signedIn(t, addr, dev)
	_ = old.ssh.Close()
	waitFor(t, func() bool { return r.sessionOf(t, tok).AttachedConn == "" }, "the dropped session was not detached")

	c := r.resumer(t, addr, dev)
	if _, err := c.call("remote.resume", tok); err != nil {
		t.Fatalf("resume within the grace: %v", err)
	}
	if _, err := c.call("auth.whoami", tok); err != nil {
		t.Fatalf("an authenticated call after the resume: %v", err)
	}
	if _, err := c.call("remote.resume", tok); codeOf(err) != rpc.CodeAlreadySignedIn {
		t.Fatalf("a second resume on the resumed connection: %v; want CodeAlreadySignedIn", err)
	}
	if s := r.sessionOf(t, tok); s.AttachedConn == "" || s.DetachedUntil != 0 {
		t.Fatalf("resumed session %+v; want attached, no longer detached", s)
	}
	counts, owner := r.eng.BeginIdleShutdown()
	if owner != 0 || counts.RemoteSessions != 1 {
		t.Fatalf("idle decision with a resumed session: %+v, owner %d; want busy", counts, owner)
	}
	if n := auditRows(t, r.store, "login"); n != 2 {
		t.Fatalf("login rows %d; a resume is not a sign-in (want bootstrap's and the first sign-in's)", n)
	}
}

// Resume BEFORE the old connection's close write (the old one is half-open):
// the new connection takes the session over, the old one is ended, and its
// requests fail the owner check.
func TestAResumeTakesOverFromAStillAttachedConnection(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	old, tok := r.signedIn(t, addr, dev)
	oldConn := r.sessionOf(t, tok).AttachedConn

	c := r.resumer(t, addr, dev)
	if _, err := c.call("remote.resume", tok); err != nil {
		t.Fatalf("resume while the old connection holds the session: %v", err)
	}
	if s := r.sessionOf(t, tok); s.AttachedConn == oldConn || s.AttachedConn == "" {
		t.Fatalf("session attached to %q; want the new connection", s.AttachedConn)
	}
	if _, err := old.call("auth.whoami", tok); err == nil {
		t.Fatal("the old connection's request passed the owner check after the takeover")
	}
	if !old.ended(t) {
		t.Fatal("the connection the session was taken from was not ended")
	}
	// Its close write, run as it ended, matched nothing: the new owner holds.
	time.Sleep(100 * time.Millisecond)
	if s := r.sessionOf(t, tok); s.AttachedConn == "" || s.DetachedUntil != 0 {
		t.Fatalf("the old connection's close write detached the new owner: %+v", s)
	}
	if _, err := c.call("auth.whoami", tok); err != nil {
		t.Fatalf("the new owner lost the session: %v", err)
	}
}

// A close write delayed past a takeover changes nothing, and a close write
// that never happened (the store was down) is followed by a takeover all the
// same.
func TestLateAndMissingCloseWritesCannotDetachTheNewOwner(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	_, tok := r.signedIn(t, addr, dev)
	sess := r.sessionOf(t, tok)
	oldConn := sess.AttachedConn

	c := r.resumer(t, addr, dev)
	if _, err := c.call("remote.resume", tok); err != nil {
		t.Fatal(err)
	}
	// The old connection's close write, delayed until after the takeover.
	if _, err := r.svc.DetachRemoteSession(sess.ID, oldConn, time.Minute); err != nil {
		t.Fatal(err)
	}
	if s := r.sessionOf(t, tok); s.AttachedConn == "" || s.AttachedConn == oldConn || s.DetachedUntil != 0 {
		t.Fatalf("a late close write moved the session: %+v", s)
	}

	// Now the store "loses" the new connection's close write: it ends, but
	// the row still names it. A resume takes it over from the dead one.
	liveConn := r.sessionOf(t, tok).AttachedConn
	_ = c.ssh.Close()
	waitFor(t, func() bool { return r.sessionOf(t, tok).AttachedConn == "" }, "not detached")
	if err := r.store.Sessions.OnCtx(t.Context()).With(meta.SessID, sess.ID).
		Set(meta.SessAttachedConn, liveConn).Set(meta.SessDetachedUntil, int64(0)).Update(); err != nil {
		t.Fatal(err)
	}
	d := r.resumer(t, addr, dev)
	if _, err := d.call("remote.resume", tok); err != nil {
		t.Fatalf("resume after a close write that never happened: %v", err)
	}
	if _, err := d.call("auth.whoami", tok); err != nil {
		t.Fatalf("whoami after taking over from a dead connection: %v", err)
	}
}

// Past the grace the resume is refused as unavailable: not counted, and the
// same connection signs in with the passphrase.
func TestAnExpiredResumeFallsBackToSignIn(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	old, tok := r.signedIn(t, addr, dev)
	_ = old.ssh.Close()
	waitFor(t, func() bool { return r.sessionOf(t, tok).AttachedConn == "" }, "not detached")
	if err := r.store.Sessions.OnCtx(t.Context()).With(meta.SessTokenHash, tokenHashOf(tok)).
		Set(meta.SessDetachedUntil, time.Now().Add(-time.Second).Unix()).Update(); err != nil {
		t.Fatal(err)
	}
	c := r.resumer(t, addr, dev)
	if _, err := c.call("remote.resume", tok); codeOf(err) != rpc.CodeResumeUnavailable {
		t.Fatalf("resume past the grace: %v; want CodeResumeUnavailable", err)
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d; an expired resume is not counted", n)
	}
	if _, err := c.login("root", "root-passphrase"); err != nil {
		t.Fatalf("signing in on the same connection after the refused resume: %v", err)
	}
	counts, owner := r.eng.BeginIdleShutdown()
	if owner != 0 || counts.RemoteSessions != 1 {
		t.Fatalf("idle counts %+v, owner %d; want one remote session (the refused resume holds none)", counts, owner)
	}
}

// Another device's token is a counted denial, and ends the connection; so is
// a resume on a connection that proved no enrolled device.
func TestAResumeWithAnotherDevicesTokenIsCounted(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	_, tok := r.signedIn(t, addr, newDevice(t))

	other := r.addKey(t, 1)
	c := r.connect(t, addr, other, "")
	dev2 := newDevice(t)
	if _, err := c.attest(dev2); err != nil {
		t.Fatal(err)
	}
	// dev2 is not enrolled yet: a resume needs an enrolled device.
	if _, err := c.call("remote.resume", tok); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("resume without an enrolled device: %v; want CodeRemoteDenied", err)
	}
	denials(t, r.store, 1)

	e := r.connect(t, addr, other, "")
	if _, err := e.attest(dev2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.login("root", "root-passphrase"); err != nil {
		t.Fatal(err)
	}
	_ = e.ssh.Close()
	f := r.connect(t, addr, other, "")
	if at, err := f.attest(dev2); err != nil || at["enrolled"] != true {
		t.Fatalf("proving dev2: %v %#v", err, at)
	}
	if _, err := f.call("remote.resume", tok); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("resume of the first device's token from dev2: %v; want CodeRemoteDenied", err)
	}
	if rows := denials(t, r.store, 2); !contains(rows[1].Detail, "protocol_violation") {
		t.Fatalf("denial %q; want protocol_violation", rows[1].Detail)
	}
	_, _ = f.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if !f.ended(t) {
		t.Fatal("the connection stayed open")
	}
}

// While a restart has closed admission, a resume is refused as restarting,
// uncounted; the session stays detached for a later resume.
func TestAResumeWhileRestartingIsRefusedUncounted(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	old, tok := r.signedIn(t, addr, dev)
	_ = old.ssh.Close()
	var owner uint64
	waitFor(t, func() bool { _, owner = r.eng.BeginIdleShutdown(); return owner != 0 }, "still busy")
	c := r.resumer(t, addr, dev)
	if _, err := c.call("remote.resume", tok); codeOf(err) != rpc.CodeServerRestarting {
		t.Fatalf("resume while restarting: %v; want CodeServerRestarting", err)
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d; want 0", n)
	}
	if s := r.sessionOf(t, tok); s.AttachedConn != "" {
		t.Fatalf("the refused resume took the session: %+v", s)
	}
	r.eng.AbortIdleShutdown(owner)
	if _, err := c.call("remote.resume", tok); err != nil {
		t.Fatalf("resume once admission reopened: %v", err)
	}
}

// Rotating the device key: both keys sign, the server swaps the key and
// resets its age in one step (logged as enrollment, rotated), the new key is
// the device from then on, and the old one is another device's. A proof that
// does not verify changes nothing. A key past its age is reported due.
func TestRotatingTheDeviceKey(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	c, tok := r.signedIn(t, addr, dev)
	devID := r.sessionOf(t, tok).DeviceID

	if err := r.store.RemoteDevices.OnCtx(t.Context()).With(meta.DevID, devID).
		Set(meta.DevKeyCreatedAt, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	due := r.connect(t, addr, r.key, "")
	if at, err := due.attest(dev); err != nil || at["rotate_due"] != true {
		t.Fatalf("a key made at 1970: %v %#v; want rotate_due", err, at)
	}

	next := newDevice(t)
	msg := remote.RotateMessage(c.ssh.SessionID(), devID, dev.pub, next.pub)
	if _, err := c.call("remote.rotate_device", tok, []byte(next.pub), ed25519.Sign(next.priv, msg), ed25519.Sign(next.priv, msg)); codeOf(err) == 0 {
		t.Fatal("a rotation the old key did not sign was accepted")
	}
	if _, err := c.call("remote.rotate_device", tok, []byte(next.pub), ed25519.Sign(dev.priv, msg), ed25519.Sign(dev.priv, msg)); codeOf(err) == 0 {
		t.Fatal("a rotation the new key did not sign was accepted")
	}
	before := auditRows(t, r.store, "remote_device_enrolled")
	res, err := c.call("remote.rotate_device", tok, []byte(next.pub), ed25519.Sign(dev.priv, msg), ed25519.Sign(next.priv, msg))
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if m, _ := res.(map[string]any); m["key_created_at"].(int64) <= 1 {
		t.Fatalf("rotation reply %#v; want the new key's time", res)
	}
	if n := auditRows(t, r.store, "remote_device_enrolled"); n != before+1 {
		t.Fatalf("remote_device_enrolled rows %d, want %d", n, before+1)
	}
	row, err := r.store.Audit.OnCtx(t.Context()).With(meta.AuditAction, "remote_device_enrolled").
		WithPredicate(dao.Like(string(meta.AuditDetail), "rotated%")).Get()
	if err != nil {
		t.Fatalf("the rotation's event: %v", err)
	}
	_ = row
	if n := liveDevices(t, r.store, 1); n != 1 {
		t.Fatalf("live devices %d after rotation, want 1 (the same device)", n)
	}

	fresh := r.connect(t, addr, r.key, "")
	if at, err := fresh.attest(next); err != nil || at["enrolled"] != true || at["rotate_due"] != false {
		t.Fatalf("proving the new key: %v %#v; want enrolled, not due", err, at)
	}
	stale := r.connect(t, addr, r.key, "")
	if _, err := stale.attest(dev); codeOf(err) != rpc.CodeRemoteDenied {
		t.Fatalf("proving the old key after rotation: %v; want CodeRemoteDenied", err)
	}
}

// A resume from an address the device has not used writes remote_new_ip, as
// a sign-in does.
func TestAResumeFromANewAddressIsLogged(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	dev := newDevice(t)
	old, tok := r.signedIn(t, addr, dev)
	_ = old.ssh.Close()
	waitFor(t, func() bool { return r.sessionOf(t, tok).AttachedConn == "" }, "not detached")
	c := r.connect(t, addr, r.key, "127.0.0.2")
	if _, err := c.attest(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := c.call("remote.resume", tok); err != nil {
		t.Fatal(err)
	}
	if n := auditRows(t, r.store, "remote_new_ip"); n != 1 {
		t.Fatalf("remote_new_ip rows %d, want 1", n)
	}
}
