package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
	"github.com/yongjohnlee80/autodb/tui/remotedial"
)

func remoteSession(t *testing.T, srv *remotetest.Server) (*Session, *int) {
	t.Helper()
	p, keys := srv.Profile(t)
	asked := 0
	d := &remotedial.Dialer{Profile: p, Keys: keys, ClientVersion: "test",
		ConfirmHostKey: func(fp string) bool { asked++; return fp == srv.HostFP }}
	return NewRemoteSession(d, logger.Nop{}), &asked
}

func rctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// whoami is an authenticated call through the session.
func whoami(t *testing.T, s *Session) error {
	t.Helper()
	_, err := s.Bind().authed(rctx(t), "auth.whoami")
	return err
}

// Remote › Connect asks for the passphrase once: the device key opens, the
// device is proved, and the same passphrase signs in as the profile's user.
// The session never starts a daemon, and the host key is pinned: a reconnect
// is not asked to confirm it again.
func TestARemoteSessionConnectsWithOnePassphrase(t *testing.T) {
	srv := remotetest.Start(t)
	s, asked := remoteSession(t, srv)
	if _, err := s.Connect(rctx(t)); !errors.Is(err, ErrRemoteNeedsUnlock) {
		t.Fatalf("Connect before the passphrase: %v; want ErrRemoteNeedsUnlock", err)
	}
	fp, err := s.ConnectRemote(rctx(t), remotetest.AlicePass)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if fp != srv.HostFP || s.Token() == "" || s.User().Name != "alice" || !s.Remote() || s.CanSpawn() {
		t.Fatalf("fp %q token %q user %+v remote %v spawn %v", fp, s.Token(), s.User(), s.Remote(), s.CanSpawn())
	}
	if err := whoami(t, s); err != nil {
		t.Fatalf("an authenticated call: %v", err)
	}
	if *asked != 1 {
		t.Fatalf("host key confirmations %d, want 1", *asked)
	}
	if p, _ := s.RemoteProfile(); p.HostKeyFP != srv.HostFP {
		t.Fatalf("the session's profile is not pinned: %q", p.HostKeyFP)
	}
}

// SPC x on a remote session (Disconnect, Connect) reconnects within the grace
// without asking anything: the device key in memory, the token resumed.
func TestAReconnectResumesTheSessionWithoutAsking(t *testing.T) {
	srv := remotetest.Start(t)
	s, asked := remoteSession(t, srv)
	if _, err := s.ConnectRemote(rctx(t), remotetest.AlicePass); err != nil {
		t.Fatal(err)
	}
	tok := s.Token()
	s.Disconnect()
	// Straight away: the old connection's close write may or may not have
	// landed, and the resume takes the session either way (detached, or
	// over from the old connection of the same device).
	if _, err := s.Connect(rctx(t)); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if s.Token() != tok {
		t.Fatal("the reconnect did not resume the session")
	}
	if err := whoami(t, s); err != nil {
		t.Fatalf("an authenticated call after the resume: %v", err)
	}
	if *asked != 1 {
		t.Fatalf("host key confirmations %d, want 1: the reconnect asked again", *asked)
	}
}

// Past the grace the resume is refused: the token is dropped, and the
// sign-in prompt signs in on the same connection as the profile's user;
// another name typed is refused.
func TestAnExpiredSessionSignsInAgainOnTheSameConnection(t *testing.T) {
	srv := remotetest.Start(t)
	s, _ := remoteSession(t, srv)
	if _, err := s.ConnectRemote(rctx(t), remotetest.AlicePass); err != nil {
		t.Fatal(err)
	}
	s.Disconnect()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := srv.Store.Sessions.OnCtx(t.Context()).With(meta.SessAttachedConn, "").
			With(meta.SessRevoked, int64(0)).With(meta.SessUserID, int64(2)).Count()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session was never detached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := srv.Store.Sessions.OnCtx(t.Context()).With(meta.SessUserID, int64(2)).
		Set(meta.SessDetachedUntil, int64(1)).Update(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(rctx(t)); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if s.Token() != "" {
		t.Fatal("an expired session's token was kept")
	}
	if err := s.Bind().Login(rctx(t), "root", remotetest.AlicePass); err == nil {
		t.Fatal("signing in as root on alice's profile was accepted")
	}
	if err := s.Bind().Login(rctx(t), "alice", remotetest.AlicePass); err != nil {
		t.Fatalf("sign-in on the reconnected connection: %v", err)
	}
	if s.User().Name != "alice" || whoami(t, s) != nil {
		t.Fatalf("signed in as %+v; want alice, the profile's user", s.User())
	}
}

// Remote › Disconnect forgets the token and wipes the device key: reaching
// the server again needs the passphrase.
func TestForgetRemoteWipesTheKeyAndTheToken(t *testing.T) {
	srv := remotetest.Start(t)
	s, _ := remoteSession(t, srv)
	if _, err := s.ConnectRemote(rctx(t), remotetest.AlicePass); err != nil {
		t.Fatal(err)
	}
	key := s.remote.key
	s.ForgetRemote()
	if s.Token() != "" || s.Connected() || s.remote.key != nil {
		t.Fatal("ForgetRemote left the token, the connection or the key")
	}
	for _, b := range key {
		if b != 0 {
			t.Fatal("the device key was not wiped in memory")
		}
	}
	if _, err := s.Connect(rctx(t)); !errors.Is(err, ErrRemoteNeedsUnlock) {
		t.Fatalf("Connect after ForgetRemote: %v; want ErrRemoteNeedsUnlock", err)
	}
}
