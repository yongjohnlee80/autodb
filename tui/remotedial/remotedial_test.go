package remotedial

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

type server struct {
	*remotetest.Server
	store  *meta.Store
	hostFP string
	key    ssh.Signer
}

const alicePass = remotetest.AlicePass

func newServer(t *testing.T) *server {
	t.Helper()
	s := remotetest.Start(t)
	return &server{Server: s, store: s.Store, hostFP: s.HostFP, key: s.Key}
}

// dialer is alice's profile against s, with a fresh key directory.
func (s *server) dialer(t *testing.T) *Dialer {
	t.Helper()
	p, keys := s.Profile(t)
	return &Dialer{Profile: p, Keys: keys, ClientVersion: "test",
		ConfirmHostKey: func(fp string) bool { return fp == s.hostFP }}
}

func (s *server) count(t *testing.T, action string) uint64 {
	t.Helper()
	n, err := s.store.Audit.OnCtx(t.Context()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// The first connect from a machine: the host key is confirmed and pinned,
// the fresh device key is staged before the sign-in, and the sign-in that
// enrolls it promotes it. The next connect proves the enrolled device.
func TestTheFirstConnectEnrollsAndTheNextProvesTheDevice(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	confirmed := ""
	d.ConfirmHostKey = func(fp string) bool { confirmed = fp; return fp == s.hostFP }
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if confirmed != s.hostFP || c.HostKeyFP() != s.hostFP || c.Enrolled() || !d.Keys.Exists(d.Keys.Pending()) {
		t.Fatalf("confirmed %q, pin %q, enrolled %v, pending %v", confirmed, c.HostKeyFP(), c.Enrolled(), d.Keys.Exists(d.Keys.Pending()))
	}
	res, err := c.SignIn(ctx(t), alicePass)
	if err != nil || res["token"] == "" {
		t.Fatalf("sign-in: %v %v", res, err)
	}
	if d.Keys.Exists(d.Keys.Pending()) || !d.Keys.Exists(d.Keys.Key()) {
		t.Fatal("the enrolled key was not promoted")
	}
	c.Close()

	d.Profile.HostKeyFP = s.hostFP
	d.ConfirmHostKey = nil // pinned: never asked again
	c2, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil || !c2.Enrolled() {
		t.Fatalf("second dial: enrolled %v, %v", c2 != nil && c2.Enrolled(), err)
	}
	if _, err := c2.SignIn(ctx(t), alicePass); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	c2.Close()
	if n := s.count(t, "remote_device_enrolled"); n != 1 {
		t.Fatalf("remote_device_enrolled rows %d, want 1", n)
	}
}

// A pinned host key that does not match, or a first host key the user does
// not confirm, ends the connect before anything autodb-level is sent.
func TestTheHostKeyPinIsHeld(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	d.Profile.HostKeyFP = "SHA256:not-the-servers"
	if _, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass}); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("a mismatched pin: %v; want ErrHostKeyMismatch", err)
	}
	d.Profile.HostKeyFP = ""
	d.ConfirmHostKey = func(string) bool { return false }
	if _, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass}); !errors.Is(err, ErrHostKeyRejected) {
		t.Fatalf("an unconfirmed first host key: %v; want ErrHostKeyRejected", err)
	}
	if s.count(t, "remote_access_denied")+s.count(t, "login_failed")+s.count(t, "login") != 1 {
		// 1: bootstrap's own sign-in row
		t.Fatal("something autodb-level reached the server")
	}
}

// On an enrolled machine a wrong passphrase fails to open the device key, and
// nothing is sent: no refusal is counted, no sign-in is tried.
func TestAWrongPassphraseOnAnEnrolledDeviceSendsNothing(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
	c.Close()
	before := s.count(t, "login") + s.count(t, "login_failed")
	d.Profile.HostKeyFP = s.hostFP
	if _, err := d.Dial(ctx(t), Unlock{Passphrase: "not-it"}); !errors.Is(err, remoteclient.ErrWrongPassphrase) {
		t.Fatalf("a wrong passphrase: %v; want ErrWrongPassphrase", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := s.count(t, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d, want 0", n)
	}
	if n := s.count(t, "login") + s.count(t, "login_failed"); n != before {
		t.Fatalf("sign-in rows %d, were %d: a sign-in was tried", n, before)
	}
}

// On a first connect a wrong passphrase can only be judged by the server: it
// is refused (and counted), and the staged key is discarded.
func TestAFirstConnectsWrongPassphraseDiscardsTheStagedKey(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: "not-it"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.SignIn(ctx(t), "not-it"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a first sign-in with a wrong passphrase: %v; want ErrRefused", err)
	}
	if d.Keys.Exists(d.Keys.Pending()) || d.Keys.Exists(d.Keys.Key()) {
		t.Fatal("the refused sign-in left a device key on disk")
	}
}

// The TUI died between the server enrolling the key and the rename: the next
// connect opens the Pending key, the server knows it, and it is promoted.
func TestACrashBeforeThePromotionIsRecovered(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := os.Rename(d.Keys.Key(), d.Keys.Pending()); err != nil { // as if never promoted
		t.Fatal(err)
	}
	d.Profile.HostKeyFP = s.hostFP
	c2, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil || !c2.Enrolled() {
		t.Fatalf("dial after the crash: %v", err)
	}
	defer c2.Close()
	if !d.Keys.Exists(d.Keys.Key()) || d.Keys.Exists(d.Keys.Pending()) {
		t.Fatal("the pending key was not promoted")
	}
	if _, err := c2.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
}

// A reconnect within the grace takes the session up with the key kept in
// memory and the token, without the passphrase.
func TestAReconnectResumesWithoutThePassphrase(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SignIn(ctx(t), alicePass)
	if err != nil {
		t.Fatal(err)
	}
	tok := res["token"].(string)
	key := append(ed25519.PrivateKey(nil), c.DeviceKey()...)
	c.Close()
	d.Profile.HostKeyFP = s.hostFP
	deadline := time.Now().Add(3 * time.Second)
	for {
		c2, err := d.Dial(ctx(t), Unlock{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := c2.Resume(ctx(t), tok)
		if rerr == nil {
			if _, err := c2.Client().Call(ctx(t), "auth.whoami", tok); err != nil {
				t.Fatalf("whoami after resume: %v", err)
			}
			c2.Close()
			return
		}
		c2.Close()
		if !errors.Is(rerr, ErrResumeUnavailable) || time.Now().After(deadline) {
			t.Fatalf("resume: %v", rerr)
		}
		time.Sleep(20 * time.Millisecond) // the old connection's close write
	}
}

// A key past its age is rotated at sign-in: the key file then holds the new
// key, and the next connect proves it.
func TestAnOldDeviceKeyIsRotatedAtSignIn(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
	old := append(ed25519.PrivateKey(nil), c.DeviceKey()...)
	c.Close()
	if err := s.store.RemoteDevices.OnCtx(t.Context()).With(meta.DevUserID, int64(2)).
		Set(meta.DevKeyCreatedAt, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	d.Profile.HostKeyFP = s.hostFP
	c2, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c2.SignIn(ctx(t), alicePass)
	if err != nil || res["rotation_error"] != nil {
		t.Fatalf("sign-in with rotation: %v %v", res, err)
	}
	c2.Close()
	now, err := d.Keys.Read(d.Keys.Key(), alicePass, s.hostFP, ssh.FingerprintSHA256(s.key.PublicKey()))
	if err != nil || bytes.Equal(now, old) || d.Keys.Exists(d.Keys.Next()) {
		t.Fatalf("after rotation the key file: %v, same %v, next left %v", err, bytes.Equal(now, old), d.Keys.Exists(d.Keys.Next()))
	}
	c3, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil || !c3.Enrolled() {
		t.Fatalf("dial with the rotated key: %v", err)
	}
	c3.Close()
}

// A rotation interrupted after the server swapped the key (the new key only
// in Next): the connect proves Key, is refused, retries with Next, and Next
// becomes the key.
func TestAnInterruptedRotationRecoversFromTheSideFile(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
	c.Close()
	oldBlob, _ := os.ReadFile(d.Keys.Key())
	if err := s.store.RemoteDevices.OnCtx(t.Context()).With(meta.DevUserID, int64(2)).
		Set(meta.DevKeyCreatedAt, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	d.Profile.HostKeyFP = s.hostFP
	c2, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.SignIn(ctx(t), alicePass); err != nil { // rotates: Key is new now
		t.Fatal(err)
	}
	c2.Close()
	// As if the TUI died after the server swapped and before the rename:
	// the new key in Next, the old one still in Key.
	if err := os.Rename(d.Keys.Key(), d.Keys.Next()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Keys.Key(), oldBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	c3, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil || !c3.Enrolled() {
		t.Fatalf("dial after the interrupted rotation: %v", err)
	}
	c3.Close()
	if d.Keys.Exists(d.Keys.Next()) {
		t.Fatal("the side file was left")
	}
	if n := s.count(t, "remote_access_denied"); n != 1 {
		t.Fatalf("remote_access_denied rows %d; want the one refusal of the old key", n)
	}
}

// With use_agent the agent signs for the profile's key, and only that key;
// an agent without it is named.
func TestTheAgentSignsForTheProfilesKeyOnly(t *testing.T) {
	s := newServer(t)
	ring := agent.NewKeyring()
	pemBytes, _ := os.ReadFile(s.KeyFile)
	raw, err := ssh.ParseRawPrivateKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	_ = ring.Add(agent.AddedKey{PrivateKey: other})
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(ring, conn) }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	d := s.dialer(t)
	d.Profile.UseAgent = true
	if _, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass}); !errors.Is(err, ErrKeyNotInAgent) {
		t.Fatalf("an agent without the profile's key: %v; want ErrKeyNotInAgent", err)
	}
	_ = ring.Add(agent.AddedKey{PrivateKey: raw})
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatalf("dial through the agent: %v", err)
	}
	defer c.Close()
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
}

// A rotation that never reached the server leaves a Next key nobody knows:
// Key proves itself, and the stray Next is removed.
func TestAStrayNextKeyIsRemovedWhenKeyProvesItself(t *testing.T) {
	s := newServer(t)
	d := s.dialer(t)
	c, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(ctx(t), alicePass); err != nil {
		t.Fatal(err)
	}
	c.Close()
	stray, _ := remoteclient.NewDeviceKey()
	if err := d.Keys.Write(d.Keys.Next(), stray, alicePass, s.hostFP, ssh.FingerprintSHA256(s.key.PublicKey())); err != nil {
		t.Fatal(err)
	}
	d.Profile.HostKeyFP = s.hostFP
	c2, err := d.Dial(ctx(t), Unlock{Passphrase: alicePass})
	if err != nil || !c2.Enrolled() {
		t.Fatalf("dial: %v", err)
	}
	c2.Close()
	if d.Keys.Exists(d.Keys.Next()) {
		t.Fatal("the stray Next key was left")
	}
	if n := s.count(t, "remote_access_denied"); n != 0 {
		t.Fatalf("remote_access_denied rows %d; Key proved first, nothing was refused", n)
	}
}
