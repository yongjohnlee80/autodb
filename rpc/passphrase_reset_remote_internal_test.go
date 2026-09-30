package rpc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// An admin's passphrase reset ends the user's remote connections, unless the
// user holds an open transaction: then none are ended. The transaction check
// is forced both ways here; the engine's own reading has its cell in exec.
func TestAPassphraseResetEndsRemoteConnectionsOnlyOutsideATransaction(t *testing.T) {
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store)
	if err != nil {
		t.Fatal(err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", auth.LocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := svc.CreateUser(ctx, rootTok, "alice", "alice-passphrase-long", meta.RoleReader, auth.LocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	var mu sync.Mutex
	var closes []*remote.Peer
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	s := New(svc, eng, config.Server{}, "t", WithListener(ln),
		WithRemoteClose(func(match func(*remote.Peer) bool) int {
			mu.Lock()
			defer mu.Unlock()
			alicePeer := &remote.Peer{UserID: alice}
			bobPeer := &remote.Peer{UserID: alice + 100}
			for _, p := range []*remote.Peer{alicePeer, bobPeer} {
				if match(p) {
					closes = append(closes, p)
				}
			}
			return len(closes)
		}))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- s.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })

	cli, srv := net.Pipe()
	ln.conns <- srv
	c, err := golibrpc.Dial(ctx, "pipe", msgpackrpc.New(nil),
		golibrpc.WithConnDialer(func(context.Context, string, string) (net.Conn, error) { return cli, nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	call := func(method string, params ...any) error {
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		defer ccancel()
		_, err := c.Call(cctx, method, params...)
		return err
	}
	if err := call("sys.hello", map[string]any{"protocol": Protocol}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		inTx      bool
		wantEnded int
	}{{inTx: true, wantEnded: 0}, {inTx: false, wantEnded: 1}} {
		mu.Lock()
		closes = nil
		mu.Unlock()
		s.userInTransaction = func(id int64) bool { return tc.inTx && id == alice }
		if err := call("auth.passphrase_reset", rootTok, alice, "alice-new-passphrase"); err != nil {
			t.Fatalf("reset (in transaction %v): %v", tc.inTx, err)
		}
		mu.Lock()
		got := len(closes)
		mu.Unlock()
		if got != tc.wantEnded {
			t.Fatalf("in transaction %v: %d connection(s) ended, want %d (and only alice's)", tc.inTx, got, tc.wantEnded)
		}
	}
}
