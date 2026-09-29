package remote_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/autodb/core/remote"
)

type registered struct{ keyID, userID int64 }

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type server struct {
	l      *remote.Listener
	hostFP string
	host   ssh.PublicKey
}

// startListener serves a remote listener on loopback whose registered keys
// are keys (fingerprint → owner).
func startListener(t *testing.T, keys map[string]registered, tune func(*remote.Config)) *server {
	t.Helper()
	host, fp, err := remote.LoadOrCreateHostKey(filepath.Join(t.TempDir(), "k", "host"))
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := remote.Config{
		HostKey: host, HostKeyFP: fp, Version: "test",
		HandshakeTimeout: 2 * time.Second,
		Authorize: func(_ context.Context, fingerprint string) (int64, int64, error) {
			r, ok := keys[fingerprint]
			if !ok {
				return 0, 0, remote.ErrUnknownKey
			}
			return r.keyID, r.userID, nil
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	l, err := remote.Listen(tcp, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &server{l: l, hostFP: fp, host: host.PublicKey()}
}

func (s *server) dial(t *testing.T, auth ...ssh.AuthMethod) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", s.l.Addr().String(), &ssh.ClientConfig{
		User: "autodb", Auth: auth, HostKeyCallback: ssh.FixedHostKey(s.host), Timeout: 2 * time.Second,
	})
}

// accept returns the next remote connection, or fails after a while.
func (s *server) accept(t *testing.T) net.Conn {
	t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		c, err := s.l.Accept()
		if err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		t.Cleanup(func() { _ = c.Close() })
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no remote connection was accepted")
		return nil
	}
}

// noAccept proves nothing reaches Accept for a while.
func (s *server) noAccept(t *testing.T) {
	t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		if c, err := s.l.Accept(); err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		c.Close()
		t.Fatal("a connection reached the RPC side")
	case <-time.After(300 * time.Millisecond):
	}
}

// openRPC opens the autodb-rpc subsystem and returns its stdin and stdout.
func openRPC(t *testing.T, c *ssh.Client) (io.WriteCloser, io.Reader) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem(remote.Subsystem); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	return in, out
}

// A registered key opens the subsystem, the RPC side gets a remote Conn whose
// Peer names that key, its user, this SSH session and the client's address,
// and bytes flow both ways.
func TestARegisteredKeyReachesTheRPCSide(t *testing.T) {
	k := newSigner(t)
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {11, 7}}, nil)
	c, err := s.dial(t, ssh.PublicKeys(k))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	in, out := openRPC(t, c)
	nc := s.accept(t)
	rc, ok := nc.(remote.Conn)
	if !ok {
		t.Fatalf("Accept returned %T, not a remote.Conn", nc)
	}
	p := rc.RemotePeer()
	if p.SSHKeyID != 11 || p.UserID != 7 || p.HostKeyFP != s.hostFP || len(p.ConnID) != 32 {
		t.Fatalf("peer %+v", p)
	}
	if string(p.SessionID) != string(c.SessionID()) {
		t.Fatal("the peer's session id is not this SSH session's")
	}
	if p.Addr.String() != c.LocalAddr().String() || nc.RemoteAddr().String() != c.LocalAddr().String() {
		t.Fatalf("addresses: peer %v, conn %v; want the client's %v", p.Addr, nc.RemoteAddr(), c.LocalAddr())
	}
	go func() { _, _ = in.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(nc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("client → server: %q, %v", buf, err)
	}
	go func() { _, _ = nc.Write([]byte("pong")) }()
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("server → client: %q, %v", buf, err)
	}
}

// An unregistered key, and a password, never authenticate.
func TestOnlyARegisteredKeyAuthenticates(t *testing.T) {
	k := newSigner(t)
	s := startListener(t, map[string]registered{}, nil)
	if c, err := s.dial(t, ssh.PublicKeys(k)); err == nil {
		c.Close()
		t.Fatal("an unregistered key authenticated")
	}
	if c, err := s.dial(t, ssh.Password("hunter2")); err == nil {
		c.Close()
		t.Fatal("a password authenticated")
	}
	s.noAccept(t)
}

// The connection's identity is the key that AUTHENTICATED (CVE-2024-45337).
// The client first offers a key nobody registered, which the server is asked
// about and refuses, then authenticates with key B (registered to user 2):
// the Peer names key 2 and user 2.
//
// What this cell cannot show: the wire order that makes the CVE's misuse
// bite (ask about one key, then sign with another). x/crypto's client asks
// about and signs with one key at a time, and its server drops the connection
// on a signature that fails, so no x/crypto client can produce it. The
// listener is safe against it by construction: its callback keeps no state,
// and the Peer is built from ssh.ServerConn.Permissions, the permissions of
// the authentication that succeeded, only (peerOf).
func TestTheIdentityIsTheKeyThatAuthenticated(t *testing.T) {
	stranger, b := newSigner(t), newSigner(t)
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(b.PublicKey()): {2, 2}}, nil)
	c, err := s.dial(t, ssh.PublicKeys(stranger, b))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	openRPC(t, c)
	p := s.accept(t).(remote.Conn).RemotePeer()
	if p.SSHKeyID != 2 || p.UserID != 2 {
		t.Fatalf("peer %+v; want key 2, user 2 (the key that signed)", p)
	}
}

// A shell, a command and a terminal are refused, and asking ends the
// connection before anything reaches the RPC side.
func TestShellExecAndPtyAreRefused(t *testing.T) {
	k := newSigner(t)
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {1, 1}}, nil)
	for name, ask := range map[string]func(*ssh.Session) error{
		"shell": func(se *ssh.Session) error { return se.Shell() },
		"exec":  func(se *ssh.Session) error { return se.Start("psql") },
		"pty":   func(se *ssh.Session) error { return se.RequestPty("xterm", 24, 80, ssh.TerminalModes{}) },
		"other subsystem": func(se *ssh.Session) error {
			return se.RequestSubsystem("sftp")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := s.dial(t, ssh.PublicKeys(k))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			se, err := c.NewSession()
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			if err := ask(se); err == nil {
				t.Fatalf("%s was granted", name)
			}
			s.noAccept(t)
		})
	}
}

// Port forwarding is refused both ways, and a second channel after the
// subsystem ends the connection.
func TestForwardingAndASecondChannelAreRefused(t *testing.T) {
	k := newSigner(t)
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {1, 1}}, nil)
	c, err := s.dial(t, ssh.PublicKeys(k))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if ln, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		ln.Close()
		t.Fatal("remote port forwarding (tcpip-forward) was granted")
	}
	openRPC(t, c)
	nc := s.accept(t)
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("a direct-tcpip channel was opened")
	}
	// The second channel ended the connection: the RPC side reads EOF. A
	// timeout would mean the connection is still open, so it does not count.
	_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = nc.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the connection survived a second channel: %v", err)
	}
}

// A connection that never finishes its handshake is closed at the deadline,
// and a full handshake cap closes the next connection at once.
func TestHandshakesAreBoundedInTimeAndNumber(t *testing.T) {
	s := startListener(t, map[string]registered{}, func(c *remote.Config) {
		c.HandshakeTimeout = 300 * time.Millisecond
		c.MaxUnauthenticated = 1
	})
	idle, err := net.Dial("tcp", s.l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(50 * time.Millisecond) // the idle one holds the only slot
	extra, err := net.Dial("tcp", s.l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := extra.Read(make([]byte, 64)); err == nil || n > 0 {
		t.Fatalf("a connection past the cap was served: %d bytes, %v", n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("a connection past the cap was left open rather than closed")
	}
	// The idle one: its banner arrives, then it is closed at the deadline.
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	start := time.Now()
	for {
		if _, err := idle.Read(buf); err != nil {
			break
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the idle handshake lasted %v; want it closed near 300ms", d)
	}
}

// The bridged connection honours a read deadline, which is how the RPC
// server wakes its read loop to drain.
func TestTheBridgeHonoursDeadlines(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := remote.Bridge(a, &net.TCPAddr{}, &net.TCPAddr{})
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read past the deadline: %v; want a timeout", err)
	}
}

// failingTCP is a TCP listener whose Accept fails, permanently, once told to.
type failingTCP struct {
	net.Listener
	fail chan struct{}
}

func (f *failingTCP) Accept() (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	got := make(chan res, 1)
	go func() { c, err := f.Listener.Accept(); got <- res{c, err} }()
	select {
	case r := <-got:
		return r.c, r.err
	case <-f.fail:
		return nil, errors.New("injected: the socket went away")
	}
}

// A listener whose TCP Accept fails stops and says why (Done, Err), so its
// supervisor can start another; one stopped by Close says nothing.
func TestAListenerThatFailsStopsAndSaysWhy(t *testing.T) {
	host, fp, err := remote.LoadOrCreateHostKey(filepath.Join(t.TempDir(), "k", "host"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := remote.Config{HostKey: host, HostKeyFP: fp,
		Authorize: func(context.Context, string) (int64, int64, error) { return 0, 0, remote.ErrUnknownKey }}
	tcp, _ := net.Listen("tcp", "127.0.0.1:0")
	ft := &failingTCP{Listener: tcp, fail: make(chan struct{})}
	l, err := remote.Listen(ft, cfg)
	if err != nil {
		t.Fatal(err)
	}
	close(ft.fail)
	select {
	case <-l.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a failed listener did not stop")
	}
	if l.Err() == nil {
		t.Fatal("a failed listener's Err is nil")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after the failure: %v; want net.ErrClosed", err)
	}

	tcp2, _ := net.Listen("tcp", "127.0.0.1:0")
	l2, _ := remote.Listen(tcp2, cfg)
	_ = l2.Close()
	<-l2.Done()
	if l2.Err() != nil {
		t.Fatalf("a closed listener's Err: %v; want nil", l2.Err())
	}
}

type denial struct {
	ip, reason, fp string
	user           int64
}

type denials struct {
	mu  sync.Mutex
	got []denial
}

func (d *denials) record(ip, reason, fp string, user int64) {
	d.mu.Lock()
	d.got = append(d.got, denial{ip, reason, fp, user})
	d.mu.Unlock()
}

func (d *denials) list() []denial {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]denial(nil), d.got...)
}

func waitDenials(t *testing.T, d *denials, n int) []denial {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := d.list(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("denials %+v; want %d", d.list(), n)
	return nil
}

// Admit is asked before a byte of SSH: a refused address gets no handshake.
func TestARefusedAddressGetsNoHandshake(t *testing.T) {
	k := newSigner(t)
	var asked []string
	var mu sync.Mutex
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {1, 1}}, func(c *remote.Config) {
		c.Admit = func(ip string) bool { mu.Lock(); asked = append(asked, ip); mu.Unlock(); return false }
	})
	if c, err := s.dial(t, ssh.PublicKeys(k)); err == nil {
		c.Close()
		t.Fatal("a refused address completed a handshake")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "127.0.0.1" {
		t.Fatalf("Admit was asked %v; want once, about 127.0.0.1", asked)
	}
}

// A failed authentication is a denial naming the key offered; a protocol
// violation after authenticating is one naming the user; a handshake that
// simply times out is none.
func TestTheListenerReportsWhatItRefused(t *testing.T) {
	k, stranger := newSigner(t), newSigner(t)
	d := &denials{}
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {5, 9}}, func(c *remote.Config) {
		c.Denied = d.record
		c.HandshakeTimeout = 300 * time.Millisecond
	})
	if c, err := s.dial(t, ssh.PublicKeys(stranger)); err == nil {
		c.Close()
		t.Fatal("an unregistered key authenticated")
	}
	got := waitDenials(t, d, 1)
	if got[0] != (denial{"127.0.0.1", remote.DeniedKey, ssh.FingerprintSHA256(stranger.PublicKey()), 0}) {
		t.Fatalf("auth failure reported as %+v", got[0])
	}
	c, err := s.dial(t, ssh.PublicKeys(k))
	if err != nil {
		t.Fatal(err)
	}
	se, _ := c.NewSession()
	_ = se.Shell()
	got = waitDenials(t, d, 2)
	if got[1].reason != remote.DeniedProtocol || got[1].user != 9 {
		t.Fatalf("a shell request reported as %+v; want a protocol violation by user 9", got[1])
	}
	c.Close()
	idle, err := net.Dial("tcp", s.l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(600 * time.Millisecond) // past the handshake deadline
	if n := len(d.list()); n != 2 {
		t.Fatalf("a timed-out handshake was counted: %+v", d.list())
	}
}

// Hangup, as the RPC server calls it after a violation, ends the SSH session:
// the client's channel sees the end.
func TestHangupEndsTheSession(t *testing.T) {
	k := newSigner(t)
	s := startListener(t, map[string]registered{ssh.FingerprintSHA256(k.PublicKey()): {1, 1}}, nil)
	c, err := s.dial(t, ssh.PublicKeys(k))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, out := openRPC(t, c)
	s.accept(t).(remote.Conn).RemotePeer().Hangup()
	done := make(chan error, 1)
	go func() { _, err := out.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the session is still open after Hangup")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session is still open after Hangup")
	}
}
