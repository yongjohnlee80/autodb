package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
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
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fan := remote.NewFanIn(local)
	srv := rpc.New(svc, eng, config.Server{}, "t", rpc.WithListener(fan), rpc.WithNotesDir("/srv/notes"))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })

	cfg := config.Default()
	cfg.Remote.Bind = "127.0.0.1:0"
	cfg.Remote.HostKey = filepath.Join(t.TempDir(), "keys", "remote_host_ed25519")
	return &remoteRig{cfg: cfg, store: store, svc: svc, fan: fan, key: key}
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
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.fan, "t", func(string) {})
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
	addr, stop, err := startRemote(t.Context(), r.cfg, r.store, r.svc, r.fan, "t", func(string) {})
	defer stop()
	if err != nil || addr != nil {
		t.Fatalf("startRemote with the switch off: %v, %v; want nothing started", addr, err)
	}
}
