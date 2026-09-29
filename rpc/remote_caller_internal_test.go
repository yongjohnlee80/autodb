package rpc

import (
	"context"
	"net"
	"testing"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// testCallerVerb is served only by the test's own server. It is admitted
// before sign-in so a remote connection can reach it; set once, before any
// test runs, so no test reads the set while it changes.
const testCallerVerb = "test.caller"

func init() { remotePreLogin[testCallerVerb] = true }

// A handler registered through handle sees its connection's Caller: remote,
// with the connection's id, on a connection the remote listener accepted,
// and local on a local connection of the same server.
func TestAHandlerSeesItsConnectionsCaller(t *testing.T) {
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	s := New(nil, nil, config.Server{}, "t", WithListener(ln))
	seen := make(chan auth.Caller, 1)
	s.handle(testCallerVerb, func(ctx context.Context, _ *golibrpc.Request) (any, error) {
		seen <- auth.CallerFrom(ctx)
		return true, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-errc })

	callerOver := func(wrap func(net.Conn) net.Conn) auth.Caller {
		t.Helper()
		cli, srv := net.Pipe()
		ln.conns <- wrap(srv)
		c, err := golibrpc.Dial(ctx, "pipe", msgpackrpc.New(nil),
			golibrpc.WithConnDialer(func(context.Context, string, string) (net.Conn, error) { return cli, nil }))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		defer ccancel()
		if _, err := c.Call(cctx, "sys.hello", map[string]any{"protocol": Protocol}); err != nil {
			t.Fatalf("hello: %v", err)
		}
		if _, err := c.Call(cctx, testCallerVerb); err != nil {
			t.Fatalf("%s: %v", testCallerVerb, err)
		}
		return <-seen
	}
	rem := callerOver(func(c net.Conn) net.Conn { return pipeRemote{Conn: c, peer: &remote.Peer{ConnID: "conn-r1"}} })
	if rem.Surface != auth.SurfaceRemote || rem.ConnID != "conn-r1" || rem.DeviceID != 0 {
		t.Fatalf("remote connection's caller %+v; want remote, conn-r1, no device", rem)
	}
	noPeer := callerOver(func(c net.Conn) net.Conn { return pipeRemote{Conn: c} })
	if noPeer.Surface != auth.SurfaceRemote || noPeer.ConnID != "" {
		t.Fatalf("a remote connection without a Peer: caller %+v; want remote with no id", noPeer)
	}
	if loc := callerOver(func(c net.Conn) net.Conn { return c }); loc != auth.LocalCaller {
		t.Fatalf("local connection's caller %+v; want LocalCaller", loc)
	}
}
