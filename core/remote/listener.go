package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Subsystem is the only SSH subsystem the remote listener serves: autodb RPC.
const Subsystem = "autodb-rpc@autodb"

// The handshake's permissions carry the authenticated key under these names.
const (
	extKeyID  = "autodb-ssh-key-id"
	extUserID = "autodb-user-id"
)

// ErrUnknownKey is what an Authorize answers for a key that is not a live
// registered key of an enabled user.
var ErrUnknownKey = errors.New("remote: not a registered SSH key")

// Why the listener refused a connection, as it tells Config.Denied. The
// limiter counts them; a capacity refusal, a handshake timeout, and a client
// that simply left are none of them, and are never counted.
const (
	// DeniedKey: the client authenticated with no registered key.
	DeniedKey = "key_not_registered"
	// DeniedProtocol: the client asked for something the listener does not
	// serve (a shell, a command, a terminal, forwarding, another channel).
	DeniedProtocol = "protocol_violation"
)

// Authorize answers whose live registered key has fingerprint (SHA256:…), or
// ErrUnknownKey. It is asked for every key a client offers.
type Authorize func(ctx context.Context, fingerprint string) (keyID, userID int64, err error)

// Config is what a remote listener needs.
type Config struct {
	// HostKey is the server's identity; HostKeyFP its SHA256 fingerprint,
	// which clients pin.
	HostKey   ssh.Signer
	HostKeyFP string
	// Authorize resolves an offered key.
	Authorize Authorize
	// MaxUnauthenticated caps connections still in their handshake; one more
	// is closed at once. Zero means 16.
	MaxUnauthenticated int
	// HandshakeTimeout bounds the handshake: authentication and the subsystem
	// request. Zero means 10s.
	HandshakeTimeout time.Duration
	// Version goes into the server's SSH banner: SSH-2.0-autodb_<version>.
	Version string
	// Logf receives the listener's operational messages; nil discards them.
	Logf func(format string, args ...any)
	// Admit, when set, is asked about every new TCP connection before a byte
	// of SSH: false closes it at once, uncounted (a blocked address, or
	// admission paused).
	Admit func(ip string) bool
	// Registry, when set, is where every connection handed to the RPC side
	// is registered for its life, so it can be ended from outside.
	Registry *Registry
	// HangupForce bounds a graceful hangup: a connection whose client stops
	// reading, so its pending bytes never leave, is closed after it. Zero
	// means 30s.
	HangupForce time.Duration
	// Denied, when set, is told of every connection the listener refused
	// for the client's own doing: the address, why (DeniedKey,
	// DeniedProtocol), the last key it offered, and the user its key named
	// once it had authenticated. It is told once per connection.
	Denied func(ip, reason, offeredKeyFP string, userID int64)
}

// Listener is the remote listener: it accepts SSH connections on a TCP
// listener and hands the RPC server one Conn per connection that
// authenticated with a registered key and asked for the autodb subsystem.
//
// It speaks nothing else. There is no password or keyboard-interactive
// authentication, no shell, exec, pty, agent or X11 forwarding, no port
// forwarding in either direction, and one subsystem channel per connection;
// anything else is refused, and a second channel ends the connection.
type Listener struct {
	tcp    net.Listener
	cfg    Config
	server *ssh.ServerConfig

	ready chan net.Conn
	done  chan struct{}
	once  sync.Once
	// regMu orders registration against stop: a connection is registered
	// before the listener stops, or it is closed instead. Whoever ends the
	// registry's connections after Close therefore reaches every one this
	// listener let through, including those still in their handshake.
	regMu  sync.Mutex
	closed bool
	unauth chan struct{} // one token per connection still in its handshake
	// err is why the listener stopped on its own (its TCP listener failed);
	// nil when Close stopped it. Written before done closes.
	err error
}

// Listen starts serving tcp. Close stops accepting; connections already
// handed to Accept are the RPC server's to close.
func Listen(tcp net.Listener, cfg Config) (*Listener, error) {
	if cfg.HostKey == nil || cfg.Authorize == nil {
		return nil, errors.New("remote: a listener needs a host key and an Authorize")
	}
	if cfg.MaxUnauthenticated <= 0 {
		cfg.MaxUnauthenticated = 16
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.HangupForce <= 0 {
		cfg.HangupForce = 30 * time.Second
	}
	l := &Listener{
		tcp: tcp, cfg: cfg,
		ready:  make(chan net.Conn),
		done:   make(chan struct{}),
		unauth: make(chan struct{}, cfg.MaxUnauthenticated),
	}
	l.server = &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-autodb_" + bannerSafe(cfg.Version),
		MaxAuthTries:  6,
		// Asked for every key the client offers. What it returns for one key
		// is that key's; the connection's identity is taken from the
		// permissions of the authentication that SUCCEEDED
		// (ssh.ServerConn.Permissions), never from state kept across these
		// calls, which is the misuse behind CVE-2024-45337.
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.HandshakeTimeout)
			defer cancel()
			keyID, userID, err := cfg.Authorize(ctx, ssh.FingerprintSHA256(key))
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{
				extKeyID:  strconv.FormatInt(keyID, 10),
				extUserID: strconv.FormatInt(userID, 10),
			}}, nil
		},
	}
	l.server.AddHostKey(cfg.HostKey)
	go l.acceptLoop()
	return l, nil
}

// bannerSafe keeps the banner a single token: SSH forbids spaces in it.
func bannerSafe(v string) string {
	if v == "" {
		return "dev"
	}
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' || r == '-' {
			return '_'
		}
		return r
	}, v)
}

// Accept returns the next authenticated remote connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting. It does not close connections Accept returned.
func (l *Listener) Close() error { return l.stop(nil) }

// stop ends the listener once: cause is nil for Close, the TCP listener's
// error when it failed.
func (l *Listener) stop(cause error) error {
	var err error
	l.once.Do(func() {
		l.regMu.Lock()
		l.closed = true
		l.regMu.Unlock()
		l.err = cause
		close(l.done)
		err = l.tcp.Close()
	})
	return err
}

// Done is closed when the listener stops: by Close, or because its TCP
// listener failed. A supervisor waits on it; Err says which.
func (l *Listener) Done() <-chan struct{} { return l.done }

// Err is why the listener stopped on its own, or nil if it is running or Close
// stopped it. Read it after Done is closed.
func (l *Listener) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// Addr is the TCP address the listener is bound to.
func (l *Listener) Addr() net.Addr { return l.tcp.Addr() }

// HostKeyFingerprint is the fingerprint clients pin.
func (l *Listener) HostKeyFingerprint() string { return l.cfg.HostKeyFP }

func (l *Listener) acceptLoop() {
	for {
		c, err := l.tcp.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// TERMINAL: the listener stops and says why, so the one watching
			// it can start another. Logging and returning left Accept blocked
			// and the remote surface gone with nothing to notice.
			l.cfg.Logf("remote listener: accept: %v", err)
			_ = l.stop(fmt.Errorf("remote listener: accept: %w", err))
			return
		}
		if l.cfg.Admit != nil && !l.cfg.Admit(hostOf(c.RemoteAddr())) {
			// Blocked, or admission paused: closed before a byte of SSH,
			// and not counted again.
			_ = c.Close()
			continue
		}
		select {
		case l.unauth <- struct{}{}:
			go l.serve(c)
		default:
			// The handshake cap is full: closed before a byte of SSH. Not the
			// peer's failure, so nothing is charged to it.
			l.cfg.Logf("remote listener: %d handshakes in progress, refusing %s", cap(l.unauth), c.RemoteAddr())
			_ = c.Close()
		}
	}
}

func (l *Listener) serve(c net.Conn) {
	released := false
	release := func() {
		if !released {
			released = true
			<-l.unauth
		}
	}
	defer release()

	_ = c.SetDeadline(time.Now().Add(l.cfg.HandshakeTimeout))
	ip := hostOf(c.RemoteAddr())
	// A per-connection copy of the server config, so the key this client
	// offered last can be named in its denial. Only named: the connection's
	// identity still comes from the permissions of the authentication that
	// succeeded (peerOf), never from this.
	var offered string
	cfg := *l.server
	cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		offered = ssh.FingerprintSHA256(key)
		return l.server.PublicKeyCallback(meta, key)
	}
	sconn, chans, reqs, err := ssh.NewServerConn(c, &cfg)
	if err != nil {
		_ = c.Close()
		// Counted only when the client tried to authenticate and failed; a
		// timeout, a dropped connection or a bad handshake is not a guess.
		var authErr *ssh.ServerAuthError
		if errors.As(err, &authErr) {
			l.denied(ip, DeniedKey, offered, 0)
		}
		return
	}
	// No global request is honoured: port forwarding (tcpip-forward) and every
	// other one is answered no.
	go ssh.DiscardRequests(reqs)

	peer, err := peerOf(sconn, l.cfg.HostKeyFP)
	if err != nil {
		l.cfg.Logf("remote listener: %s: %v", sconn.RemoteAddr(), err)
		_ = sconn.Close()
		return
	}
	ch, rest, violation := l.subsystem(sconn, chans)
	if ch == nil {
		_ = sconn.Close()
		if violation {
			l.denied(ip, DeniedProtocol, offered, peer.UserID)
		}
		return
	}
	release()
	_ = c.SetDeadline(time.Time{})

	// Any further channel ends the connection, and is counted: one subsystem
	// per connection.
	go func() {
		for nc := range rest {
			_ = nc.Reject(ssh.Prohibited, "one autodb-rpc channel per connection")
			_ = sconn.Close()
			// Counted once per connection: several opens already queued
			// when the first is refused are one violation.
			if peer.ClaimViolation() {
				l.denied(ip, DeniedProtocol, offered, peer.UserID)
			}
		}
	}()
	br := bridge(closeBoth{ch, sconn}, sconn.LocalAddr(), sconn.RemoteAddr())
	rc := &conn{Conn: br, peer: peer}
	var hangOnce sync.Once
	peer.hangup = func() {
		hangOnce.Do(func() {
			// What the RPC side wrote goes first; then the session ends.
			br.closeWhenWritten()
			// A client that stopped reading cannot hold it past this.
			time.AfterFunc(l.cfg.HangupForce, func() { _ = sconn.Close() })
		})
	}
	l.regMu.Lock()
	if l.closed {
		l.regMu.Unlock()
		_ = rc.Close()
		return
	}
	if reg := l.cfg.Registry; reg != nil {
		reg.add(peer)
		go func() {
			_ = sconn.Wait()
			reg.remove(peer.ConnID)
		}()
	}
	l.regMu.Unlock()
	select {
	case l.ready <- rc:
	case <-l.done:
		_ = rc.Close()
	}
}

// peerOf builds a connection's Peer from the handshake that succeeded.
func peerOf(sconn *ssh.ServerConn, hostFP string) (*Peer, error) {
	if sconn.Permissions == nil {
		return nil, errors.New("authenticated without permissions")
	}
	keyID, err := strconv.ParseInt(sconn.Permissions.Extensions[extKeyID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("authenticated key id: %w", err)
	}
	userID, err := strconv.ParseInt(sconn.Permissions.Extensions[extUserID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("authenticated user id: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("connection id: %w", err)
	}
	return &Peer{
		ConnID:    hex.EncodeToString(id),
		SSHKeyID:  keyID,
		UserID:    userID,
		SessionID: append([]byte(nil), sconn.SessionID()...),
		HostKeyFP: hostFP,
		Addr:      sconn.RemoteAddr(),
	}, nil
}

// subsystem waits, within the handshake deadline, for the one session channel
// and its autodb-rpc subsystem request. A channel of another type, or a
// session request for a shell, a command, a terminal, agent or X11
// forwarding, ends the connection. It returns the channel and the stream of
// any later channel opens.
//
// violation reports that the connection asked for something not served, as
// opposed to leaving or failing on its own.
func (l *Listener) subsystem(sconn *ssh.ServerConn, chans <-chan ssh.NewChannel) (ch ssh.Channel, rest <-chan ssh.NewChannel, violation bool) {
	nc, ok := <-chans
	if !ok {
		return nil, nil, false
	}
	if nc.ChannelType() != "session" {
		_ = nc.Reject(ssh.UnknownChannelType, "only an autodb-rpc session is served")
		return nil, nil, true
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		return nil, nil, false
	}
	for req := range reqs {
		switch {
		case req.Type == "subsystem" && subsystemName(req.Payload) == Subsystem:
			_ = req.Reply(true, nil)
			// Later requests on the channel are all answered no.
			go func() {
				for r := range reqs {
					_ = r.Reply(false, nil)
				}
			}()
			return ch, chans, false
		case req.Type == "env":
			// Clients send these unasked; refused, and harmless.
			_ = req.Reply(false, nil)
		default:
			// shell, exec, pty-req, x11-req, auth-agent-req@openssh.com, a
			// different subsystem: none is served, and asking ends it.
			_ = req.Reply(false, nil)
			_ = ch.Close()
			return nil, nil, true
		}
	}
	return nil, nil, false
}

// denied tells Config.Denied, when set.
func (l *Listener) denied(ip, reason, offered string, userID int64) {
	if l.cfg.Denied != nil {
		l.cfg.Denied(ip, reason, offered, userID)
	}
}

// hostOf is an address's host: the IP a TCP peer connected from.
func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// subsystemName decodes a subsystem request's payload: one SSH string.
func subsystemName(payload []byte) string {
	var p struct{ Name string }
	if err := ssh.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Name
}

// closeBoth closes the channel and then its SSH connection: a remote
// connection is one channel, so the RPC side closing it ends the connection.
type closeBoth struct {
	ssh.Channel
	sconn io.Closer
}

func (c closeBoth) Close() error {
	err := c.Channel.Close()
	_ = c.sconn.Close()
	return err
}
