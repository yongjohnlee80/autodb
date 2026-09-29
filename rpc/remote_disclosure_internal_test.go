package rpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/remote"
)

type pipeRemote struct {
	net.Conn
	peer *remote.Peer
}

func (c pipeRemote) RemotePeer() *remote.Peer { return c.peer }

// pipeListener hands the server the far ends of in-memory pipes, each wrapped
// by wrap: the listener, not the client, decides the surface.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { close(l.done); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "tcp" }
func (pipeAddr) String() string  { return "198.51.100.9:52000" }

// A server that DISCLOSES operator detail (a host-local endpoint) still withholds
// it from a remote connection: the decision is per connection. The local
// connection on the same server is the positive control.
func TestDisclosureIsWithheldFromARemoteConnectionOnADisclosingServer(t *testing.T) {
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	s := New(nil, nil, config.Server{}, "t", WithDetailDisclosure(true), WithListener(ln))
	sessions := make(chan *golibrpc.Session, 2)
	s.rpc.Handle("sys.hello", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		sessions <- req.Session
		return s.helloHandler(ctx, req)
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-errc })

	greet := func(wrap func(net.Conn) net.Conn) *golibrpc.Session {
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
		return <-sessions
	}
	remoteSess := greet(func(c net.Conn) net.Conn { return pipeRemote{Conn: c, peer: &remote.Peer{ConnID: "r1"}} })
	localSess := greet(func(c net.Conn) net.Conn { return c })

	de := exec.NewDialFailure(7, errors.New(leakyCause))
	var local, rem *golibrpc.Error
	if !errors.As(s.wireErrFor(&golibrpc.Request{Session: localSess}, de), &local) ||
		!errors.As(s.wireErrFor(&golibrpc.Request{Session: remoteSess}, de), &rem) {
		t.Fatal("wireErrFor did not answer a *golibrpc.Error")
	}
	if !strings.Contains(local.Message, "db7.internal") {
		t.Fatalf("positive control: the local connection was not shown the cause: %q", local.Message)
	}
	for _, leak := range append(append([]string{}, causeDiagnostic...), causeSecrets...) {
		if strings.Contains(rem.Message, leak) {
			t.Fatalf("the remote connection was shown %q: %q", leak, rem.Message)
		}
	}
	if rem.Code != CodeDialFailed {
		t.Errorf("remote code %d, want CodeDialFailed: withholding changes the text, not the code", rem.Code)
	}
}
