package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/rpc"
)

type remoteRig struct {
	cfg   config.Config
	store *meta.Store
	svc   *auth.Service
	lim   *auth.RemoteLimiter
	fan   *remote.FanIn
	key   ssh.Signer
}

// newRemoteRig is a store with root, root's SSH key registered, and an RPC
// server behind a fan-in over a local loopback listener.
func newRemoteRig(t *testing.T) *remoteRig {
	t.Helper()
	ctx := t.Context()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewSignerFromKey(priv)
	if _, err := store.SSHKeys.OnCtx(ctx).Set(meta.SSHKeyUserID, int64(1)).
		Set(meta.SSHKeyPublicKey, string(ssh.MarshalAuthorizedKey(key.PublicKey()))).
		Set(meta.SSHKeyFingerprint, ssh.FingerprintSHA256(key.PublicKey())).
		Set(meta.SSHKeyCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	eng := coreexec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	cfg := config.Default()
	cfg.Remote.Bind = "127.0.0.1:0"
	cfg.Remote.HostKey = filepath.Join(t.TempDir(), "keys", "remote_host_ed25519")
	cfg.Remote.DenialSpill = filepath.Join(t.TempDir(), "spill", "remote-denials.pending")
	lim, err := newRemoteLimiter(cfg, svc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lim.Close)
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fan := remote.NewFanIn(local)
	srv := rpc.New(svc, eng, config.Server{}, "t", rpc.WithListener(fan), rpc.WithNotesDir("/srv/notes"),
		rpc.WithRemoteDenials(func(ip, reason string, userID int64) {
			remoteDeny(lim, func(string) {}, ip, reason, "", userID)
		}))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })

	return &remoteRig{cfg: cfg, store: store, svc: svc, lim: lim, fan: fan, key: key}
}

// rpcOverSSH dials addr with the rig's key, opens the autodb subsystem, and
// returns an RPC client speaking over it.
func (r *remoteRig) rpcOverSSH(t *testing.T, addr net.Addr) *golibrpc.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{
		User: "autodb", Auth: []ssh.AuthMethod{ssh.PublicKeys(r.key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{remote.Subsystem})); err != nil || !ok {
		t.Fatalf("subsystem: %v, %v", ok, err)
	}
	conn := remote.Bridge(ch, c.LocalAddr(), c.RemoteAddr())
	cli, err := golibrpc.Dial(t.Context(), "ssh", msgpackrpc.New(nil),
		golibrpc.WithConnDialer(func(context.Context, string, string) (net.Conn, error) { return conn, nil }))
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// With Remote Control on, startRemote serves the remote listener into the
// fan-in: a registered key reaches the RPC server as the REMOTE surface (the
// greeting has no notes_dir; whoami is refused until the remote sign-in).
func TestStartRemoteServesTheRemoteSurfaceWhenRemoteControlIsOn(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.lim, r.fan, "t", func(string) {})
	if err != nil || addr == nil {
		t.Fatalf("startRemote: %v, %v", addr, err)
	}
	t.Cleanup(stop)
	cli := r.rpcOverSSH(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hello, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if m, _ := hello.(map[string]any); m == nil {
		t.Fatalf("hello: %#v", hello)
	} else if _, has := m["notes_dir"]; has {
		t.Fatalf("the remote greeting carries notes_dir: %#v", m)
	}
	_, err = cli.Call(ctx, "auth.whoami", "any-token")
	var re *golibrpc.Error
	if !errors.As(err, &re) || re.Code != rpc.CodeRemoteLoginRequired {
		t.Fatalf("whoami over the remote surface: %v; want CodeRemoteLoginRequired", err)
	}
}

// With Remote Control off (the default), nothing listens.
func TestStartRemoteDoesNothingWhileRemoteControlIsOff(t *testing.T) {
	r := newRemoteRig(t)
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.lim, r.fan, "t", func(string) {})
	defer stop()
	if err != nil || addr != nil {
		t.Fatalf("startRemote with the switch off: %v, %v; want nothing started", addr, err)
	}
}

// failOnce is a TCP listener whose Accept fails, permanently, when told to.
type failOnce struct {
	net.Listener
	fail chan struct{}
}

func (f *failOnce) Accept() (net.Conn, error) {
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

// SUPERVISED: the remote listener's TCP listener failing does not leave the
// remote surface gone. The failure is said loudly, the listener is bound
// again after the backoff, and a registered key reaches the RPC server
// through the new one. Not parallel: it replaces the bind seam.
func TestAFailedRemoteListenerIsRestarted(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	first := &failOnce{fail: make(chan struct{})}
	rebound := make(chan net.Addr, 4)
	calls := 0
	oldListen, oldFirst := remoteListen, remoteRetryFirst
	t.Cleanup(func() { remoteListen, remoteRetryFirst = oldListen, oldFirst })
	remoteRetryFirst = 20 * time.Millisecond
	remoteListen = func(network, addr string) (net.Listener, error) {
		calls++
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		if calls == 1 {
			first.Listener = ln
			return first, nil
		}
		rebound <- ln.Addr()
		return ln, nil
	}
	var mu sync.Mutex
	var logged []string
	onLog := func(m string) { mu.Lock(); logged = append(logged, m); mu.Unlock() }
	_, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.lim, r.fan, "t", onLog)
	if err != nil {
		t.Fatalf("startRemote: %v", err)
	}
	t.Cleanup(stop)

	close(first.fail)
	var addr net.Addr
	select {
	case addr = <-rebound:
	case <-time.After(3 * time.Second):
		t.Fatal("the failed remote listener was never bound again")
	}
	cli := r.rpcOverSSH(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol}); err != nil {
		t.Fatalf("hello through the restarted listener: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logged, func(m string) bool { return strings.Contains(m, "REMOTE LISTENER FAILED") }) {
		t.Errorf("the failure was not said: %q", logged)
	}
}

func auditRows(t *testing.T, store *meta.Store, action string) uint64 {
	t.Helper()
	n, err := store.Audit.OnCtx(t.Context()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// End to end: three connections with an unregistered key are counted, each
// recorded, and the address is then refused before a byte of SSH, even with a
// registered key.
func TestThreeFailedRemoteLoginsBlockTheAddress(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.lim, r.fan, "t", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	stranger, _ := ssh.NewSignerFromKey(priv)
	dial := func(k ssh.Signer) error {
		c, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb",
			Auth: []ssh.AuthMethod{ssh.PublicKeys(k)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
		if err == nil {
			c.Close()
		}
		return err
	}
	for i := 0; i < 3; i++ {
		if dial(stranger) == nil {
			t.Fatal("an unregistered key authenticated")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for auditRows(t, r.store, "remote_access_denied") < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 3 {
		t.Fatalf("remote_access_denied rows %d, want 3", n)
	}
	if err := dial(r.key); err == nil {
		t.Fatal("a blocked address completed a handshake with a registered key")
	}
}

// End to end: a remote connection's call of a method it may not make before
// signing in is refused, counted, and the connection ends after the refusal.
func TestARemoteProtocolViolationIsCountedAndHungUp(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.lim, r.fan, "t", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	cli := r.rpcOverSSH(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_, err = cli.Call(ctx, "conn.list", "any-token")
	var re *golibrpc.Error
	if !errors.As(err, &re) || re.Code != rpc.CodeRemoteLoginRequired {
		t.Fatalf("conn.list: %v; want the refusal delivered", err)
	}
	select {
	case <-cli.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the connection was not hung up after the violation")
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 1 {
		t.Fatalf("remote_access_denied rows %d, want 1", n)
	}
}

// Without its limiter the remote listener does not start: a remote surface
// with no bound on guesses is refused.
func TestStartRemoteRefusesToRunWithoutItsLimiter(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, nil, r.fan, "t", func(string) {})
	defer stop()
	if err == nil || addr != nil {
		t.Fatalf("startRemote without a limiter: %v, %v; want it refused", addr, err)
	}
}
