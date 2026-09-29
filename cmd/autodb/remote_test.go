package main

import (
	"bufio"
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

	"github.com/yongjohnlee80/golib/msgpack"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/core/remotectl"
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
	lim, err := remotectl.NewLimiter(cfg, svc)
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
			remotectl.Deny(lim, func(string) {}, ip, reason, "", userID)
		}))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })

	return &remoteRig{cfg: cfg, store: store, svc: svc, lim: lim, fan: fan, key: key}
}

// control is Remote Control over the rig, with what it logs collected.
func (r *remoteRig) control(t *testing.T, lim *auth.RemoteLimiter, tweak func(*remotectl.Config)) (*remotectl.Control, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var logged []string
	cfg := remotectl.Config{Cfg: r.cfg, Store: r.store, Auth: r.svc, Limiter: lim, Fan: r.fan, Version: "t",
		Logf: func(m string) { mu.Lock(); logged = append(logged, m); mu.Unlock() }}
	if tweak != nil {
		tweak(&cfg)
	}
	ctl := remotectl.New(cfg)
	if err := ctl.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(ctl.Close)
	return ctl, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(logged) }
}

// waitState waits for Remote Control to reach state.
func waitState(t *testing.T, ctl *remotectl.Control, state string) remotectl.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := ctl.Status()
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote control %+v; want %s", st, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listening starts Remote Control, whose switch is on, and returns where it
// listens.
func (r *remoteRig) listening(t *testing.T) net.Addr {
	t.Helper()
	ctl, _ := r.control(t, r.lim, nil)
	st := waitState(t, ctl, remotectl.StateListening)
	addr, err := net.ResolveTCPAddr("tcp", st.Addr)
	if err != nil {
		t.Fatal(err)
	}
	return addr
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

// With Remote Control on, the daemon serves the remote listener into the
// fan-in: a registered key reaches the RPC server as the REMOTE surface (the
// greeting has no notes_dir; whoami is refused until the remote sign-in).
func TestRemoteControlServesTheRemoteSurfaceWhenItIsOn(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr := r.listening(t)
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
func TestRemoteControlDoesNothingWhileItIsOff(t *testing.T) {
	r := newRemoteRig(t)
	listened := false
	ctl, _ := r.control(t, r.lim, func(c *remotectl.Config) {
		c.Listen = func(network, addr string) (net.Listener, error) { listened = true; return net.Listen(network, addr) }
	})
	time.Sleep(50 * time.Millisecond)
	if st := ctl.Status(); st.State != remotectl.StateOff || st.On || listened {
		t.Fatalf("remote control with the switch off: %+v, bound %v; want nothing started", st, listened)
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
// through the new one.
func TestAFailedRemoteListenerIsRestarted(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	first := &failOnce{fail: make(chan struct{})}
	rebound := make(chan net.Addr, 4)
	calls := 0
	ctl, logged := r.control(t, r.lim, func(c *remotectl.Config) {
		c.RetryFirst = 20 * time.Millisecond
		c.Listen = func(network, addr string) (net.Listener, error) {
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
	})
	waitState(t, ctl, remotectl.StateListening)

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
	if !slices.ContainsFunc(logged(), func(m string) bool { return strings.Contains(m, "REMOTE LISTENER FAILED") }) {
		t.Errorf("the failure was not said: %q", logged())
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
	addr := r.listening(t)
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
// signing in is refused and counted, and the connection's next request ends it.
func TestARemoteProtocolViolationIsCountedAndHungUp(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr := r.listening(t)
	cli := r.rpcOverSSH(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_, err := cli.Call(ctx, "conn.list", "any-token")
	var re *golibrpc.Error
	if !errors.As(err, &re) || re.Code != rpc.CodeRemoteLoginRequired {
		t.Fatalf("conn.list: %v; want the refusal delivered", err)
	}
	// Its next request ends it: the refusal was answered, and no further
	// guess is made on this connection.
	_, _ = cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol})
	select {
	case <-cli.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the connection was not hung up after its next request")
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 1 {
		t.Fatalf("remote_access_denied rows %d, want 1", n)
	}
}

// Without its limiter the remote listener does not serve: a remote surface
// with no bound on guesses is refused, and the status says why.
func TestRemoteControlRefusesToListenWithoutItsLimiter(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	listened := false
	ctl, _ := r.control(t, nil, func(c *remotectl.Config) {
		c.Listen = func(network, addr string) (net.Listener, error) { listened = true; return net.Listen(network, addr) }
	})
	time.Sleep(50 * time.Millisecond)
	st := ctl.Status()
	if st.State != remotectl.StateRetrying || !strings.Contains(st.Err, "limiter") || listened {
		t.Fatalf("remote control without a limiter: %+v, bound %v; want it refused", st, listened)
	}
}

// A remote violation's refusal is written before the connection ends, however
// slowly the client reads it; and the connection's NEXT request ends it. Here
// the client waits 600ms before reading the refusal (a timer that hung up at a
// fixed delay would already have closed the session), then sends again and
// the session ends. One violation is counted.
func TestARemoteRefusalArrivesBeforeTheHangupAndTheNextRequestEndsIt(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr := r.listening(t)
	c, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(r.key)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{remote.Subsystem})); err != nil || !ok {
		t.Fatalf("subsystem: %v %v", ok, err)
	}
	br := bufio.NewReader(ch)
	send := func(id int64, method string, params ...any) {
		b, err := msgpack.Marshal([]any{int64(0), id, method, params})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Write(b); err != nil {
			t.Fatalf("write %s: %v", method, err)
		}
	}
	read := func() ([]any, error) {
		v, err := msgpack.Decode(br, nil)
		if err != nil {
			return nil, err
		}
		arr, _ := v.([]any)
		return arr, nil
	}
	send(1, "sys.hello", map[string]any{"protocol": rpc.Protocol})
	if _, err := read(); err != nil {
		t.Fatalf("hello: %v", err)
	}
	send(2, "conn.list", "any-token")
	time.Sleep(600 * time.Millisecond) // a slow reader
	arr, err := read()
	if err != nil || len(arr) != 4 {
		t.Fatalf("the refusal was lost before a slow reader read it: %v, %#v", err, arr)
	}
	if m, _ := arr[2].(map[string]any); m["code"] != rpc.CodeRemoteLoginRequired {
		t.Fatalf("refusal %#v; want CodeRemoteLoginRequired", arr[2])
	}
	send(3, "sys.hello", map[string]any{"protocol": rpc.Protocol})
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := read(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the next request did not end the connection")
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 1 {
		t.Fatalf("remote_access_denied rows %d, want 1", n)
	}
}

// PIPELINED: the violating request and the next one arrive in one write. The
// refusal to the first is still delivered, then the session ends; one
// violation is counted.
//
// What this cell cannot show: a refusal stuck behind a FULL SSH receive
// window. x/crypto's client opens channels with a 2 MiB window and offers no
// way to shrink it, so a small refusal is always sent at once, and an
// immediate close here passes too. The ordering under a blocked channel write
// is proven in core/remote (TestAGracefulCloseWaitsForBlockedAndQueuedWrites),
// where the channel's writes block until released.
func TestAPipelinedNextRequestDoesNotLoseTheRefusal(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	addr := r.listening(t)
	c, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "autodb",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(r.key)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{remote.Subsystem})); err != nil || !ok {
		t.Fatalf("subsystem: %v %v", ok, err)
	}
	frame := func(id int64, method string, params ...any) []byte {
		b, err := msgpack.Marshal([]any{int64(0), id, method, params})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	br := bufio.NewReader(ch)
	if _, err := ch.Write(frame(1, "sys.hello", map[string]any{"protocol": rpc.Protocol})); err != nil {
		t.Fatal(err)
	}
	if _, err := msgpack.Decode(br, nil); err != nil {
		t.Fatalf("hello: %v", err)
	}
	pipelined := append(frame(2, "conn.list", "any-token"), frame(3, "sys.hello", map[string]any{"protocol": rpc.Protocol})...)
	if _, err := ch.Write(pipelined); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // both are in before anything is read
	v, err := msgpack.Decode(br, nil)
	if err != nil {
		t.Fatalf("the refusal to the violating request was lost: %v", err)
	}
	arr, _ := v.([]any)
	if len(arr) != 4 || arr[1] != int64(2) {
		t.Fatalf("first reply %#v; want the refusal to request 2", v)
	}
	if m, _ := arr[2].(map[string]any); m["code"] != rpc.CodeRemoteLoginRequired {
		t.Fatalf("refusal %#v", arr[2])
	}
	done := make(chan struct{})
	go func() {
		for {
			if _, err := msgpack.Decode(br, nil); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not end after the pipelined request")
	}
	if n := auditRows(t, r.store, "remote_access_denied"); n != 1 {
		t.Fatalf("remote_access_denied rows %d, want 1", n)
	}
}

// hello calls sys.hello on cli.
func hello(t *testing.T, cli *golibrpc.Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := cli.Call(ctx, "sys.hello", map[string]any{"protocol": rpc.Protocol})
	return err
}

// Turned on at run time, Remote Control records the switch, audits the
// change once (turning it on again is no change), and serves.
func TestTurningRemoteControlOnRecordsAuditsAndServes(t *testing.T) {
	r := newRemoteRig(t)
	ctl, _ := r.control(t, r.lim, nil)
	for range 2 {
		if err := ctl.Set(t.Context(), true, 1, "127.0.0.1"); err != nil {
			t.Fatalf("turn on: %v", err)
		}
	}
	if v, _, _ := r.store.GetMeta(t.Context(), remote.ControlKey); v != "on" {
		t.Fatalf("switch %q; want on", v)
	}
	if n := auditRows(t, r.store, "remote_control_changed"); n != 1 {
		t.Fatalf("remote_control_changed rows %d; want 1", n)
	}
	st := waitState(t, ctl, remotectl.StateListening)
	if !st.On || st.HostKeyFP == "" || st.Addr == "" {
		t.Fatalf("status %+v; want on, with its address and host key", st)
	}
	addr, _ := net.ResolveTCPAddr("tcp", st.Addr)
	if err := hello(t, r.rpcOverSSH(t, addr)); err != nil {
		t.Fatalf("hello over the remote surface: %v", err)
	}
}

// Turned off, Remote Control closes the listener and ends every live remote
// connection, and the local surface of the same server keeps working. The
// switch is recorded and the change audited.
func TestTurningRemoteControlOffEndsRemoteConnectionsAndSparesLocalOnes(t *testing.T) {
	r := newRemoteRig(t)
	ctl, _ := r.control(t, r.lim, nil)
	if err := ctl.Set(t.Context(), true, 1, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, ctl, remotectl.StateListening)
	addr, _ := net.ResolveTCPAddr("tcp", st.Addr)
	remoteCli := r.rpcOverSSH(t, addr)
	if err := hello(t, remoteCli); err != nil {
		t.Fatalf("remote hello: %v", err)
	}
	localCli, err := golibrpc.Dial(t.Context(), r.fan.Addr().String(), msgpackrpc.New(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localCli.Close() })
	if err := hello(t, localCli); err != nil {
		t.Fatalf("local hello: %v", err)
	}
	if n := ctl.Status().Live; n != 1 {
		t.Fatalf("live remote connections %d; want 1", n)
	}

	if err := ctl.Set(t.Context(), false, 1, "127.0.0.1"); err != nil {
		t.Fatalf("turn off: %v", err)
	}
	select {
	case <-remoteCli.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the remote connection outlived Remote Control being turned off")
	}
	if err := hello(t, localCli); err != nil {
		t.Fatalf("the local connection was touched: %v", err)
	}
	if st := ctl.Status(); st.State != remotectl.StateOff || st.On {
		t.Fatalf("status %+v; want off", st)
	}
	if c, err := net.DialTimeout("tcp", addr.String(), time.Second); err == nil {
		c.Close()
		t.Fatal("the remote listener still accepts after Remote Control was turned off")
	}
	if v, _, _ := r.store.GetMeta(t.Context(), remote.ControlKey); v != "off" {
		t.Fatalf("switch %q; want off", v)
	}
	if n := auditRows(t, r.store, "remote_control_changed"); n != 2 {
		t.Fatalf("remote_control_changed rows %d; want 2", n)
	}
}

// A first bind that fails (the port is taken) does not leave Remote Control
// silently off: the status says it is retrying and why, and already shows the
// host key's fingerprint; once the port is free the listener binds and serves.
func TestAFailedFirstBindIsRetriedAndShown(t *testing.T) {
	r := newRemoteRig(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.Remote.Bind = busy.Addr().String()
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	ctl, logged := r.control(t, r.lim, func(c *remotectl.Config) { c.RetryFirst, c.RetryMax = 20*time.Millisecond, 40*time.Millisecond })
	deadline := time.Now().Add(3 * time.Second)
	var st remotectl.Status
	for st = ctl.Status(); st.Err == "starting" || st.Err == ""; st = ctl.Status() {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v; want the bind failure", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.State != remotectl.StateRetrying || !st.On || st.NextTry.IsZero() || !strings.Contains(st.Err, "address already in use") {
		t.Fatalf("status %+v; want retrying with the bind failure", st)
	}
	if st.HostKeyFP == "" {
		t.Fatalf("status %+v; want the host key fingerprint shown while retrying, so it can be pinned", st)
	}
	if !slices.ContainsFunc(logged(), func(m string) bool { return strings.Contains(m, "REMOTE LISTENER NOT SERVING") }) {
		t.Errorf("the failure was not said: %q", logged())
	}
	_ = busy.Close()
	st = waitState(t, ctl, remotectl.StateListening)
	if st.Addr != r.cfg.Remote.Bind || st.Err != "" {
		t.Fatalf("status %+v; want listening on %s", st, r.cfg.Remote.Bind)
	}
	addr, _ := net.ResolveTCPAddr("tcp", st.Addr)
	if err := hello(t, r.rpcOverSSH(t, addr)); err != nil {
		t.Fatalf("hello after the retry: %v", err)
	}
}

// The daemon's shutdown (Close) ends the live remote connections and leaves
// the switch on, so the next start serves again.
func TestShutdownEndsRemoteConnectionsAndKeepsTheSwitch(t *testing.T) {
	r := newRemoteRig(t)
	if err := r.store.SetMeta(t.Context(), remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	ctl, _ := r.control(t, r.lim, nil)
	st := waitState(t, ctl, remotectl.StateListening)
	addr, _ := net.ResolveTCPAddr("tcp", st.Addr)
	cli := r.rpcOverSSH(t, addr)
	if err := hello(t, cli); err != nil {
		t.Fatal(err)
	}
	ctl.Close()
	select {
	case <-cli.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a remote connection outlived the shutdown")
	}
	if v, _, _ := r.store.GetMeta(t.Context(), remote.ControlKey); v != "on" {
		t.Fatalf("switch %q after shutdown; want it left on", v)
	}
}

// Turned off while its listener is still binding, Remote Control says off:
// the bind that then succeeds does not report the stopped listener as
// listening, and the listener is closed.
func TestABindFinishingAfterTurnOffLeavesItOff(t *testing.T) {
	r := newRemoteRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var bound net.Listener
	ctl, _ := r.control(t, r.lim, func(c *remotectl.Config) {
		c.Listen = func(network, addr string) (net.Listener, error) {
			close(entered)
			<-release
			ln, err := net.Listen(network, addr)
			bound = ln
			return ln, err
		}
	})
	if err := ctl.Set(t.Context(), true, 1, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	<-entered
	off := make(chan error, 1)
	go func() { off <- ctl.Set(t.Context(), false, 1, "127.0.0.1") }()
	waitState(t, ctl, remotectl.StateOff)
	close(release)
	if err := <-off; err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if st := ctl.Status(); st.State != remotectl.StateOff || st.On {
		t.Fatalf("status %+v after the late bind; want off", st)
	}
	if c, err := net.DialTimeout("tcp", bound.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("the late-bound listener was left accepting")
	}
}
