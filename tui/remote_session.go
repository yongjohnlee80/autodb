package tui

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/yongjohnlee80/golib/logger"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

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

// localTransport is a session's local transport, kept while it is switched to
// a remote server, for SwitchToLocal to restore.
type localTransport struct {
	network, addr string
	spawn         func() (string, error)
}

// SwitchToRemote moves the session onto a remote server, in place: the
// current transport is closed, the token and identity are forgotten (a new
// identity epoch), and the generation moves on, so everything issued on the
// previous transport is stale by the checks it already makes. The local
// transport is kept for SwitchToLocal, and while remote the session never
// starts a daemon. ConnectRemote then connects it.
func (s *Session) SwitchToRemote(d *remotedial.Dialer) {
	s.mu.Lock()
	old, oldConn := s.client, (*remotedial.Conn)(nil)
	if s.remote != nil {
		oldConn = s.remote.conn
		remoteclient.Wipe(s.remote.key)
	} else {
		s.local = &localTransport{network: s.network, addr: s.addr, spawn: s.spawn}
	}
	s.remote = &remoteState{d: d}
	s.network, s.addr, s.spawn = "remote", d.Profile.Address(), nil
	s.client, s.instance = nil, ""
	s.gen++
	s.forgetLoginLocked()
	s.mu.Unlock()
	closeTransport(old, oldConn)
}

// SwitchToLocal moves a remote session back onto its local transport: the
// remote connection is closed, and the token, identity and device key are
// forgotten. Reaching the local daemon needs a local sign-in. A session that
// was never local (NewRemoteSession) is only forgotten.
func (s *Session) SwitchToLocal() {
	s.mu.Lock()
	if s.remote == nil {
		s.mu.Unlock()
		return
	}
	old, oldConn := s.client, s.remote.conn
	remoteclient.Wipe(s.remote.key)
	s.remote.key, s.remote.conn = nil, nil
	if s.local != nil {
		s.network, s.addr, s.spawn = s.local.network, s.local.addr, s.local.spawn
		s.remote, s.local = nil, nil
	}
	s.client, s.instance = nil, ""
	s.gen++
	s.forgetLoginLocked()
	s.mu.Unlock()
	closeTransport(old, oldConn)
}

// forgetLoginLocked drops the token and identity, as a new identity epoch.
func (s *Session) forgetLoginLocked() {
	s.token, s.user = "", UserInfo{}
	s.idEpoch++
}

// closeTransport closes a remote connection (its SSH session too), or a
// local client.
func closeTransport(cli *golibrpc.Client, conn *remotedial.Conn) {
	if conn != nil {
		conn.Close()
		return
	}
	if cli != nil {
		_ = cli.Close()
	}
}

// NewRemoteSession is a session whose transport is a remote autodb server,
// reached over its SSH listener. It never starts a daemon.
func NewRemoteSession(d *remotedial.Dialer, log logger.Logger) *Session {
	s := NewSessionOn("remote", d.Profile.Address(), log, nil)
	s.remote = &remoteState{d: d}
	return s
}

// Remote reports whether the session's transport is a remote server.
func (s *Session) Remote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote != nil
}

// ConnectRemote is Remote › Connect: the one passphrase prompt opens the
// device key, the connection is dialed and the device proved, and the same
// passphrase signs in as the profile's user. The device key stays in memory
// for reconnects until ForgetRemote. It returns the server's host key
// fingerprint, which the caller pins in the profile on a first connect.
func (s *Session) ConnectRemote(ctx context.Context, passphrase string) (hostKeyFP string, err error) {
	rs := s.remoteSnapshot()
	if rs == nil {
		return "", errors.New("tui: not a remote session")
	}
	conn, myGen, err := s.remoteDial(ctx, rs, remotedial.Unlock{Passphrase: passphrase})
	if err != nil {
		return "", err
	}
	res, err := conn.SignIn(ctx, passphrase)
	if err != nil {
		return conn.HostKeyFP(), err
	}
	s.mu.Lock()
	if s.remote == rs {
		rs.key = append(ed25519.PrivateKey(nil), conn.DeviceKey()...)
		// Pinned from here on: a reconnect is not asked to confirm the host
		// key again, and a different one is refused.
		rs.d.Profile.HostKeyFP = conn.HostKeyFP()
	}
	s.mu.Unlock()
	s.adoptLogin(res, myGen)
	if msg, _ := res["rotation_error"].(string); msg != "" {
		s.log.Log(logger.SeverityWarning, map[string]any{"tui": "session", "event": "device key rotation failed", "error": msg})
	}
	return conn.HostKeyFP(), nil
}

// remoteSnapshot is the session's remote transport now, nil when local. The
// transport can be switched from the UI loop at any time, so each remote step
// works on one snapshot and writes back only while it is still current.
func (s *Session) remoteSnapshot() *remoteState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

// connectRemote is Connect for a remote session: dial with the device key in
// memory and, holding a token, resume it. A session that can no longer be
// resumed drops the token, so the UI's sign-in prompt signs in on this same
// connection (remoteSignIn).
func (s *Session) connectRemote(ctx context.Context) (bool, error) {
	s.mu.Lock()
	rs := s.remote
	var key ed25519.PrivateKey
	if rs != nil {
		key = rs.key
	}
	token := s.token
	s.mu.Unlock()
	if key == nil {
		return false, ErrRemoteNeedsUnlock
	}
	conn, myGen, err := s.remoteDial(ctx, rs, remotedial.Unlock{Key: key})
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

// remoteDial dials rs, installs the connection as the session's client, and
// records the greeting; superseded if the session switched transport or
// moved on meanwhile.
func (s *Session) remoteDial(ctx context.Context, rs *remoteState, u remotedial.Unlock) (*remotedial.Conn, uint64, error) {
	superseded := errors.New("tui: connect superseded by a newer transition")
	s.mu.Lock()
	if s.remote != rs {
		s.mu.Unlock()
		return nil, 0, superseded
	}
	old, oldConn := s.client, rs.conn
	s.client, rs.conn = nil, nil
	s.gen++
	myGen := s.gen
	s.mu.Unlock()
	closeTransport(old, oldConn)
	conn, err := rs.d.Dial(ctx, u)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != myGen || s.remote != rs {
		conn.Close()
		return nil, 0, superseded
	}
	h := conn.Hello()
	s.client = conn.Client()
	rs.conn = conn
	s.version, _ = h["version"].(string)
	s.serverPID, _ = h["pid"].(int64)
	s.serverAddr = rs.d.Profile.Address()
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
	var conn *remotedial.Conn
	if s.remote != nil {
		conn = s.remote.conn
	}
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
	s.mu.Lock()
	if s.remote == nil {
		s.mu.Unlock()
		return
	}
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

// RemoteDialer is the remote session's connector, nil for a local session:
// which remote connect a result belongs to.
func (s *Session) RemoteDialer() *remotedial.Dialer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remote == nil {
		return nil
	}
	return s.remote.d
}

// RemoteProfile is the profile a remote session connects, for the pin.
func (s *Session) RemoteProfile() (remoteclient.Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remote == nil {
		return remoteclient.Profile{}, false
	}
	return s.remote.d.Profile, true
}
