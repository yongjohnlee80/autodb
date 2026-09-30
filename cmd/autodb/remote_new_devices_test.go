package main

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// loginReply is a sign-in on c with the whole reply.
func loginReply(t *testing.T, c *remoteClient, name, pass string) map[string]any {
	t.Helper()
	res, err := c.call("auth.login", name, pass)
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	m, _ := res.(map[string]any)
	return m
}

// newDevicesOf is a sign-in reply's new devices, by fingerprint.
func newDevicesOf(t *testing.T, reply map[string]any) []string {
	t.Helper()
	raw, ok := reply["new_devices"].([]any)
	if !ok {
		t.Fatalf("the sign-in reply has no new_devices list: %#v", reply)
	}
	var fps []string
	for _, d := range raw {
		m, _ := d.(map[string]any)
		fp, _ := m["fingerprint"].(string)
		fps = append(fps, fp)
	}
	return fps
}

// A device enrolled since another device last signed in is told to that
// device once, at its next sign-in; a resume in between does not use the
// telling up. The enrollment's own sign-in is told nothing, a revoked
// device is never told, and a later sign-in of the newer device is not told
// of the older one.
func TestASignInIsToldOnceOfDevicesEnrolledSince(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	ctx := t.Context()
	devA := newDevice(t)

	a1, tokA := r.signedIn(t, addr, devA)
	devAID := r.sessionOf(t, tokA).DeviceID
	// Times far apart, so seconds cannot tie: A enrolled at 900 and last
	// signed in at 1000.
	if err := r.store.RemoteDevices.OnCtx(ctx).With(meta.DevID, devAID).Set(meta.DevEnrolledAt, int64(900)).Update(); err != nil {
		t.Fatal(err)
	}
	if err := r.store.Sessions.OnCtx(ctx).With(meta.SessDeviceID, devAID).Set(meta.SessCreatedAt, int64(1000)).Update(); err != nil {
		t.Fatal(err)
	}

	keyB, devB := r.addKey(t, 1), newDevice(t)
	b := r.connect(t, addr, keyB, "")
	if _, err := b.attest(devB); err != nil {
		t.Fatal(err)
	}
	if got := newDevicesOf(t, loginReply(t, b, "root", "root-passphrase")); len(got) != 0 {
		t.Fatalf("an enrollment's own sign-in was told of %v; want nothing", got)
	}
	rowB, err := r.store.RemoteDevices.OnCtx(ctx).With(meta.DevFingerprint, deviceFP(t, devB)).Get()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.RemoteDevices.OnCtx(ctx).With(meta.DevID, rowB.ID).Set(meta.DevEnrolledAt, int64(2000)).Update(); err != nil {
		t.Fatal(err)
	}
	// A revoked device, enrolled since as well, is not told.
	keyC, devC := r.addKey(t, 1), newDevice(t)
	c := r.connect(t, addr, keyC, "")
	if _, err := c.attest(devC); err != nil {
		t.Fatal(err)
	}
	_ = loginReply(t, c, "root", "root-passphrase")
	if err := r.store.RemoteDevices.OnCtx(ctx).With(meta.DevFingerprint, deviceFP(t, devC)).
		Set(meta.DevEnrolledAt, int64(2000)).Set(meta.DevRevokedAt, int64(2001)).Update(); err != nil {
		t.Fatal(err)
	}

	// A's connection drops and resumes: not a sign-in, so nothing is told
	// and nothing is used up.
	_ = a1.ssh.Close()
	waitFor(t, func() bool { return r.sessionOf(t, tokA).AttachedConn == "" }, "A's dropped session was not detached")
	a2 := r.resumer(t, addr, devA)
	if _, err := a2.call("remote.resume", tokA); err != nil {
		t.Fatalf("A's resume: %v", err)
	}
	if _, err := a2.call("auth.logout", tokA); err != nil {
		t.Fatal(err)
	}

	a3 := r.resumer(t, addr, devA)
	reply := loginReply(t, a3, "root", "root-passphrase")
	if told := newDevicesOf(t, reply); len(told) != 1 || told[0] != rowB.Fingerprint {
		t.Fatalf("A's next sign-in was told of %v; want only B (%s)", told, rowB.Fingerprint)
	}
	tok3, _ := reply["token"].(string)
	if _, err := a3.call("auth.logout", tok3); err != nil {
		t.Fatal(err)
	}

	a4 := r.resumer(t, addr, devA)
	if again := newDevicesOf(t, loginReply(t, a4, "root", "root-passphrase")); len(again) != 0 {
		t.Fatalf("A's sign-in after that was told again of %v; want nothing", again)
	}

	_ = b.ssh.Close()
	b2 := r.connect(t, addr, keyB, "")
	if _, err := b2.attest(devB); err != nil {
		t.Fatal(err)
	}
	if got := newDevicesOf(t, loginReply(t, b2, "root", "root-passphrase")); len(got) != 0 {
		t.Fatalf("B's later sign-in was told of %v; A enrolled before B", got)
	}
}

// deviceFP is dev's device fingerprint, as the server records it.
func deviceFP(t *testing.T, dev device) string {
	t.Helper()
	_, fp, err := auth.DeviceKey(dev.pub)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// A device is never told of itself, even when its enrollment and its last
// sign-in fall in the same second.
func TestASignInIsNeverToldOfItsOwnDevice(t *testing.T) {
	r := newRemoteRig(t)
	addr := r.serving(t)
	ctx := t.Context()
	dev := newDevice(t)
	c1, tok := r.signedIn(t, addr, dev)
	id := r.sessionOf(t, tok).DeviceID
	if err := r.store.RemoteDevices.OnCtx(ctx).With(meta.DevID, id).Set(meta.DevEnrolledAt, int64(5000)).Update(); err != nil {
		t.Fatal(err)
	}
	if err := r.store.Sessions.OnCtx(ctx).With(meta.SessDeviceID, id).Set(meta.SessCreatedAt, int64(5000)).Update(); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.call("auth.logout", tok); err != nil {
		t.Fatal(err)
	}
	c2 := r.resumer(t, addr, dev)
	if got := newDevicesOf(t, loginReply(t, c2, "root", "root-passphrase")); len(got) != 0 {
		t.Fatalf("a sign-in was told of %v; want nothing (not its own device)", got)
	}
}
