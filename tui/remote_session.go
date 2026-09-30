package tui

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/yongjohnlee80/golib/logger"

	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/tui/remotedial"
)

// ErrRemoteNeedsUnlock: a remote session with no device key in memory: the
// profile has to be connected with the passphrase (ConnectRemote) first.
var ErrRemoteNeedsUnlock = errors.New("tui: connect to the remote server with your passphrase first")

// remoteState is a remote session's transport: the connector, the device key
// held in memory while the profile is connected (so a reconnect resumes
// without the passphrase), and the connection now in use.
type remoteState struct {
	d    *remotedial.Dialer
	key  ed25519.PrivateKey
	conn *remotedial.Conn
}

// NewRemoteSession is a session whose transport is a remote autodb server,
// reached over its SSH listener. It never starts a daemon.
func NewRemoteSession(d *remotedial.Dialer, log logger.Logger) *Session {
	s := NewSessionOn("remote", d.Profile.Address(), log, nil)
	s.remote = &remoteState{d: d}
	return s
}

// Remote reports whether the session's transport is a remote server.
func (s *Session) Remote() bool { return s.remote != nil }

// ConnectRemote is Remote › Connect: the one passphrase prompt opens the
// device key, the connection is dialed and the device proved, and the same
// passphrase signs in as the profile's user. The device key stays in memory
// for reconnects until ForgetRemote. It returns the server's host key
// fingerprint, which the caller pins in the profile on a first connect.
func (s *Session) ConnectRemote(ctx context.Context, passphrase string) (hostKeyFP string, err error) {
	if s.remote == nil {
		return "", errors.New("tui: not a remote session")
	}
	conn, myGen, err := s.remoteDial(ctx, remotedial.Unlock{Passphrase: passphrase})
	if err != nil {
		return "", err
	}
	res, err := conn.SignIn(ctx, passphrase)
	if err != nil {
		return conn.HostKeyFP(), err
	}
	s.mu.Lock()
	s.remote.key = append(ed25519.PrivateKey(nil), conn.DeviceKey()...)
	// Pinned from here on: a reconnect is not asked to confirm the host key
	// again, and a different one is refused.
	s.remote.d.Profile.HostKeyFP = conn.HostKeyFP()
	s.mu.Unlock()
	s.adoptLogin(res, myGen)
	if msg, _ := res["rotation_error"].(string); msg != "" {
		s.log.Log(logger.SeverityWarning, map[string]any{"tui": "session", "event": "device key rotation failed", "error": msg})
	}
	return conn.HostKeyFP(), nil
}

// connectRemote is Connect for a remote session: dial with the device key in
// memory and, holding a token, resume it. A session that can no longer be
// resumed drops the token, so the UI's sign-in prompt signs in on this same
// connection (remoteSignIn).
func (s *Session) connectRemote(ctx context.Context) (bool, error) {
	s.mu.Lock()
	key := s.remote.key
	token := s.token
	s.mu.Unlock()
	if key == nil {
		return false, ErrRemoteNeedsUnlock
	}
	conn, myGen, err := s.remoteDial(ctx, remotedial.Unlock{Key: key})
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	instanceChanged := s.instanceChangedLocked(conn.Hello())
	s.mu.Unlock()
	if token == "" || instanceChanged {
		s.dropLogin(myGen)
		return instanceChanged, nil
	}
	if _, err := conn.Resume(ctx, token); err != nil {
		if errors.Is(err, remotedial.ErrResumeUnavailable) {
			s.dropLogin(myGen)
			return false, nil
		}
		return false, err
	}
	return false, nil
}

// remoteDial dials, installs the connection as the session's client, and
// records the greeting.
func (s *Session) remoteDial(ctx context.Context, u remotedial.Unlock) (*remotedial.Conn, uint64, error) {
	s.mu.Lock()
	old := s.client
	oldConn := s.remote.conn
	s.client, s.remote.conn = nil, nil
	s.gen++
	myGen := s.gen
	s.mu.Unlock()
	if oldConn != nil {
		oldConn.Close()
	} else if old != nil {
		_ = old.Close()
	}
	conn, err := s.remote.d.Dial(ctx, u)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != myGen {
		conn.Close()
		return nil, 0, errors.New("tui: connect superseded by a newer transition")
	}
	h := conn.Hello()
	s.client = conn.Client()
	s.remote.conn = conn
	s.version, _ = h["version"].(string)
	s.serverPID, _ = h["pid"].(int64)
	s.serverAddr = s.remote.d.Profile.Address()
	if inst, _ := h["instance"].(string); inst != "" && s.instance == "" {
		s.instance = inst
	}
	return conn, myGen, nil
}

// instanceChangedLocked records the greeting's instance and reports whether
// it is a new server process.
func (s *Session) instanceChangedLocked(h map[string]any) bool {
	inst, _ := h["instance"].(string)
	changed := s.instance != "" && inst != "" && s.instance != inst
	s.instance = inst
	return changed
}

// dropLogin forgets the token and identity, in epoch myGen.
func (s *Session) dropLogin(myGen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != myGen {
		return
	}
	if s.token != "" || s.user != (UserInfo{}) {
		s.token, s.user = "", UserInfo{}
		s.idEpoch++
	}
}

// remoteSignIn signs in on the remote connection in use, as the profile's
// user whatever name was typed: the remote account is the profile's (a
// remote session takes nothing from the client side).
func (s *Session) remoteSignIn(ctx context.Context, gen uint64, passphrase string) error {
	s.mu.Lock()
	conn := s.remote.conn
	s.mu.Unlock()
	if conn == nil {
		return errors.New("tui: not connected")
	}
	res, err := conn.SignIn(ctx, passphrase)
	if err != nil {
		return err
	}
	s.adoptLogin(res, gen)
	return nil
}

// ForgetRemote is Remote › Disconnect: the connection is closed, and the
// token and the device key are wiped. Reaching the server again needs the
// passphrase.
func (s *Session) ForgetRemote() {
	if s.remote == nil {
		return
	}
	s.mu.Lock()
	conn := s.remote.conn
	s.remote.conn, s.client = nil, nil
	remoteclient.Wipe(s.remote.key)
	s.remote.key = nil
	s.gen++
	if s.token != "" || s.user != (UserInfo{}) {
		s.token, s.user = "", UserInfo{}
		s.idEpoch++
	}
	s.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// RemoteProfile is the profile a remote session connects, for the pin.
func (s *Session) RemoteProfile() (remoteclient.Profile, bool) {
	if s.remote == nil {
		return remoteclient.Profile{}, false
	}
	return s.remote.d.Profile, true
}
