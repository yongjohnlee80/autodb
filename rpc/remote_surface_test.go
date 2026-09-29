package rpc_test

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/autodb/sql/deployments"
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
	return newTwoSurfacesWith(t, func(c net.Conn) *remote.Peer {
		return &remote.Peer{ConnID: "remote-1", SSHKeyID: 1, UserID: 1, Addr: c.RemoteAddr()}
	})
}

// newTwoSurfacesWith is newTwoSurfaces whose remote connections carry the
// Peer peerOf returns, nil included.
func newTwoSurfacesWith(t *testing.T, peerOf func(net.Conn) *remote.Peer) *twoSurfaces {
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
	go pump(rem, func(c net.Conn) net.Conn { return remoteConn{Conn: c, peer: peerOf(c)} })
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
	return remoteFixtureOn(t, newTwoSurfaces(t), opts...)
}

func remoteFixtureOn(t *testing.T, ln *twoSurfaces, opts ...rpc.Option) (*fixture, *client, *client) {
	t.Helper()
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

// A remote connection whose listener supplied no Peer is still remote: the
// remote restrictions apply and nothing is disclosed. Treating "no facts" as
// "local" would fail open.
func TestARemoteConnectionWithoutAPeerIsStillRemote(t *testing.T) {
	f, _, rem := remoteFixtureOn(t, newTwoSurfacesWith(t, func(net.Conn) *remote.Peer { return nil }),
		rpc.WithNotesDir("/var/lib/autodb/notes"))
	errVal, _ := rem.call("auth.whoami", f.rootTok)
	mustErr(t, errVal, rpc.CodeRemoteLoginRequired)
	errVal, _ = rem.call("sys.shutdown", f.rootTok)
	mustErr(t, errVal, rpc.CodeRemoteRefused)
	_, res := rem.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	m, _ := res.(map[string]any)
	for _, k := range []string{"notes_dir", "addr"} {
		if _, has := m[k]; has {
			t.Errorf("a Peer-less remote connection was given %s: %#v", k, m[k])
		}
	}
}

// The greeting gives a remote connection no path on this host: not the notes
// root, not the local listen address, and not the store backup the start
// took (non-empty here: the start backed an existing store up before applying
// a pending script). A local connection gets all three.
func TestTheRemoteGreetingCarriesNoHostPaths(t *testing.T) {
	ctx := context.Background()
	cfg := config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
	first, err := meta.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	updates, err := deployments.Updates(engine.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meta.RevertScript(ctx, first, updates[len(updates)-1].Number); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	store, err := meta.Open(ctx, cfg) // backs the store up, then applies the script
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.StartReport().Backup == "" {
		t.Fatal("setup: the start took no backup, so this cell would prove nothing")
	}
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln := newTwoSurfaces(t)
	srv := rpc.New(svc, eng, config.Server{}, "t", rpc.WithListener(ln), rpc.WithNotesDir("/var/lib/autodb/notes"))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })

	hello := func(addr string) map[string]any {
		c := dialAt(t, addr)
		errVal, res := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
		if errVal != nil {
			t.Fatalf("hello: %#v", errVal)
		}
		m, _ := res.(map[string]any)
		return m
	}
	local, rem := hello(ln.local.Addr().String()), hello(ln.remote.Addr().String())
	if sch, _ := local["schema"].(map[string]any); sch["backup"] == "" || local["notes_dir"] == nil || local["addr"] == nil {
		t.Fatalf("positive control: the local greeting lacks a host path: %#v", local)
	}
	for _, k := range []string{"notes_dir", "addr"} {
		if _, has := rem[k]; has {
			t.Errorf("the remote greeting carries %s: %#v", k, rem[k])
		}
	}
	sch, _ := rem["schema"].(map[string]any)
	if _, has := sch["backup"]; has {
		t.Errorf("the remote greeting carries the backup path: %#v", sch["backup"])
	}
	if _, has := sch["applied_at_start"]; !has {
		t.Errorf("the remote greeting lost applied_at_start, which names scripts, not paths: %#v", sch)
	}
}
