// Package remotetest runs a real autodb daemon with Remote Control on, for
// the cells of the TUI's remote connect: an in-memory store, root and alice
// (a reader) whose SSH key is registered, and the remote listener on
// loopback.
package remotetest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/engine"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/core/remotectl"
	"github.com/yongjohnlee80/autodb/rpc"
)

// AlicePass is alice's autodb passphrase.
const AlicePass = "alice-passphrase-long"

// Server is a daemon with Remote Control on, and alice, a reader, whose SSH
// key (in KeyFile) is registered.
type Server struct {
	Store   *meta.Store
	Svc     *auth.Service
	Addr    string
	HostFP  string
	KeyFile string
	Key     ssh.Signer
}

// Start runs one, for the life of t.
func Start(t *testing.T) *Server {
	t.Helper()
	ctx := t.Context()
	store, err := meta.Open(ctx, config.Meta{Engine: engine.SQLite, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", auth.LocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := svc.CreateUser(ctx, rootTok, "alice", AlicePass, meta.RoleReader, auth.LocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	if err := os.WriteFile(keyFile+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddSSHKey(ctx, rootTok, alice, string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "laptop", "", auth.LocalPeer); err != nil {
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
	ctl := remotectl.New(remotectl.Config{Cfg: cfg, Store: store, Auth: svc, Limiter: lim, Fan: fan, Version: "t"})
	srv := rpc.New(svc, eng, config.Server{}, "t", rpc.WithListener(fan),
		rpc.WithRemoteDenials(func(ip, reason string, userID int64) {
			remotectl.Deny(lim, func(string) {}, ip, reason, "", userID)
		}),
		rpc.WithRemoteSignIns(func(ip string) { _ = lim.Succeeded(context.Background(), ip) }),
		rpc.WithRemoteClose(ctl.Registry().Close))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })
	if err := store.SetMeta(ctx, remote.ControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	deadline := time.Now().Add(3 * time.Second)
	for ctl.Status().State != remotectl.StateListening {
		if time.Now().After(deadline) {
			t.Fatalf("remote control %+v", ctl.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := ctl.Status()
	return &Server{Store: store, Svc: svc, Addr: st.Addr, HostFP: st.HostKeyFP, KeyFile: keyFile, Key: signer}
}

// Profile is alice's profile against s, and a fresh key directory.
func (s *Server) Profile(t *testing.T) (remoteclient.Profile, remoteclient.KeyFiles) {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.Addr)
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	p := remoteclient.Profile{ID: "test", Host: host, Port: n, User: "alice", KeyFile: s.KeyFile}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	keys, err := remoteclient.FilesFor(filepath.Join(t.TempDir(), "remote"), "test")
	if err != nil {
		t.Fatal(err)
	}
	return p, keys
}

// Count is how many audit rows of action there are.
func (s *Server) Count(t *testing.T, action string) uint64 {
	t.Helper()
	n, err := s.Store.Audit.OnCtx(t.Context()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}
