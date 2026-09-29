package rpc_test

import (
	"bufio"
	"net"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/rpc"
)

// remoteConn is a connection the test's "remote listener" accepted: a plain
// TCP connection carrying a Peer, standing in for an SSH channel.
type remoteConn struct {
	net.Conn
	peer *remote.Peer
}

func (c remoteConn) RemotePeer() *remote.Peer { return c.peer }

// twoSurfaces is ONE listener serving two sources, as the daemon's will: a
// local loopback listener and a "remote" one whose connections carry a Peer.
// The server behind it is one server; only the connection decides the surface.
type twoSurfaces struct {
	local, remote net.Listener
	conns         chan net.Conn
	done          chan struct{}
	once          sync.Once
}

func newTwoSurfaces(t *testing.T) *twoSurfaces {
	t.Helper()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rem, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &twoSurfaces{local: local, remote: rem, conns: make(chan net.Conn), done: make(chan struct{})}
	pump := func(src net.Listener, wrap func(net.Conn) net.Conn) {
		for {
			c, err := src.Accept()
			if err != nil {
				return
			}
			select {
			case l.conns <- wrap(c):
			case <-l.done:
				c.Close()
				return
			}
		}
	}
	go pump(local, func(c net.Conn) net.Conn { return c })
	go pump(rem, func(c net.Conn) net.Conn {
		return remoteConn{Conn: c, peer: &remote.Peer{ConnID: "remote-1", SSHKeyID: 1, UserID: 1, Addr: c.RemoteAddr()}}
	})
	return l
}

func (l *twoSurfaces) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *twoSurfaces) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.local.Close()
		l.remote.Close()
	})
	return nil
}

func (l *twoSurfaces) Addr() net.Addr { return l.local.Addr() }

func dialAt(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func remoteFixture(t *testing.T, opts ...rpc.Option) (*fixture, *client, *client) {
	t.Helper()
	ln := newTwoSurfaces(t)
	f := newFixtureOn(t, ln, nil, opts...)
	local := dialAt(t, ln.local.Addr().String())
	rem := dialAt(t, ln.remote.Addr().String())
	local.hello()
	rem.hello()
	return f, local, rem
}

// Before it has signed in, a remote connection reaches only the greeting: every
// other method, even one a local connection may call with the same token, is
// refused at the gate as "sign in first". The same server serves the local
// connection normally.
func TestARemoteConnectionReachesOnlyTheGreetingBeforeSigningIn(t *testing.T) {
	f, local, rem := remoteFixture(t)
	for _, call := range []struct {
		method string
		params []any
	}{
		{"auth.login", []any{"root", "root-passphrase"}},
		{"auth.whoami", []any{f.rootTok}},
		{"conn.list", []any{f.rootTok}},
	} {
		errVal, _ := rem.call(call.method, call.params...)
		mustErr(t, errVal, rpc.CodeRemoteLoginRequired)
	}
	if errVal, _ := local.call("auth.whoami", f.rootTok); errVal != nil {
		t.Fatalf("the local connection on the same server was refused: %#v", errVal)
	}
}

// Restarting the daemon and claiming an empty store are refused on the remote
// surface outright, with the root token, and still work locally.
func TestTheRemoteSurfaceRefusesRestartAndBootstrap(t *testing.T) {
	f, local, rem := remoteFixture(t)
	for _, call := range []struct {
		method string
		params []any
	}{
		{"sys.shutdown", []any{f.rootTok}},
		{"sys.restart_if_idle", []any{f.rootTok}},
		{"auth.bootstrap", []any{"eve", "a long enough passphrase"}},
	} {
		errVal, _ := rem.call(call.method, call.params...)
		mustErr(t, errVal, rpc.CodeRemoteRefused)
	}
	// The local surface still reaches the handler: bootstrap on a store that
	// already has users is refused by the handler, not by the gate.
	errVal, _ := local.call("auth.bootstrap", "eve", "a long enough passphrase")
	if m, ok := errVal.(map[string]any); !ok || m["code"] == rpc.CodeRemoteRefused {
		t.Fatalf("local auth.bootstrap: %#v; want the handler's answer, not the remote refusal", errVal)
	}
}

// The greeting to a remote connection leaves out the notes directory, a path
// on this host; a local connection still gets it.
func TestTheRemoteGreetingLeavesOutTheNotesDirectory(t *testing.T) {
	_, local, rem := remoteFixture(t, rpc.WithNotesDir("/var/lib/autodb/notes"))
	_, lres := local.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	_, rres := rem.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if m, _ := lres.(map[string]any); m["notes_dir"] != "/var/lib/autodb/notes" {
		t.Fatalf("local greeting: %#v; want notes_dir", lres)
	}
	if m, _ := rres.(map[string]any); m == nil {
		t.Fatalf("remote greeting: %#v", rres)
	} else if _, has := m["notes_dir"]; has {
		t.Fatalf("remote greeting carries notes_dir: %#v", m)
	}
}

// A remote connection is told the store needs no bootstrap, whatever the
// store holds: the form must never be offered over the network.
func TestARemoteConnectionIsNeverOfferedBootstrap(t *testing.T) {
	_, _, rem := remoteFixture(t)
	errVal, need := rem.call("auth.needs_bootstrap")
	if errVal != nil || need != false {
		t.Fatalf("remote auth.needs_bootstrap: %#v, %#v; want false", errVal, need)
	}
}
