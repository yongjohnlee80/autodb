package rpc_test

// Entry-point fixture for wire cells — code-review §11 ("the wiring is the
// claim"): a verb's behavior is proven through the DISPATCH — a real
// loopback server, a real msgpack-RPC frame — not by calling the handler's
// implementation directly. A direct cell stays green when the verb is
// never registered or its wiring drops a field; a wire cell does not.
//
// The standard verb round-trip cell is five lines:
//
//	f := newFixture(t)
//	c := f.session(t)                                  // dial + hello
//	errVal, result := c.call("verb.name", f.rootTok)   // one frame each way
//	mustErr(t, errVal, rpc.CodeX) /* or assert result */
//	if f.auditCount(t, "action") == 0 { t.Error(...) } // the row it promised
//
// c.login(name, pass) turns the auth.login dance into one line for cells
// that need a non-root wire token.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/msgpack"
)

// fixture: in-memory meta store, bootstrapped auth, engine, one sqlite
// target connection, and the RPC server on a real loopback TCP port.
type fixture struct {
	store   *meta.Store
	rootTok string
	connID  int64
	eng     *exec.Engine
	addr    string
}

// frontDoorConn creates a connection a PAT may legally be bound to (ADR-0086
// §6): postgres, session profile, with a target_db derived from its DSN.
//
// Created ON DEMAND rather than in newFixture, and that is the point: adding
// it to the shared fixture changed what conn.list returns and broke an
// unrelated test that asserts the full list. A fixture shared by every test in
// a package is not the place for a row only two of them want.
//
// The DSN is parsed, never dialled — pgxpool.ParseConfig wants a well-formed
// string, not a reachable server — so this costs no live PostgreSQL.
func (f *fixture) frontDoorConn(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := f.eng.CreateConnection(ctx, f.rootTok, fmt.Sprintf("frontdoor-%d", fixtureSeq.Add(1)),
		"postgres", "postgres://u:p@127.0.0.1:5432/fixture_db?sslmode=disable", "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection(front door): %v", err)
	}
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, id).
		Set(meta.ConnProfile, meta.ProfileSession).Update(); uerr != nil {
		t.Fatalf("enabling the session profile: %v", uerr)
	}
	return id
}

var fixtureSeq atomic.Int64

// newFixture builds the shared server. The variadic options are appended to
// the ones every fixture needs, so a cell that requires a surface the others do
// not — a front door, say — asks for it WITHOUT changing what every other cell
// gets. Adding it to the shared construction is how an unrelated test starts
// failing for a reason its author cannot see.
func newFixture(t *testing.T, opts ...rpc.Option) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	dsn := fmt.Sprintf("file:rpctest%d_%d?mode=memory&cache=shared", time.Now().UnixNano(), fixtureSeq.Add(1))
	connID, err := eng.CreateConnection(ctx, rootTok, "target", "sqlite", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version",
		append([]rpc.Option{rpc.WithListener(ln)}, opts...)...)
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server Run: %v", err)
		}
	})
	deadline := time.After(2 * time.Second)
	for srv.Addr() == "" {
		select {
		case <-deadline:
			t.Fatal("server never bound")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	return &fixture{store: store, rootTok: rootTok, connID: connID, eng: eng, addr: srv.Addr()}
}

// client is a raw msgpack-RPC test client.
type client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	next int64
}

func (f *fixture) dial(t *testing.T) *client {
	t.Helper()
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn, br: bufio.NewReader(conn)}
}

// session returns an ADMITTED client: dialed and past a compatible hello.
func (f *fixture) session(t *testing.T) *client {
	t.Helper()
	c := f.dial(t)
	c.hello()
	return c
}

// call performs one request/response round-trip and returns (errVal, result).
func (c *client) call(method string, params ...any) (any, any) {
	c.t.Helper()
	c.next++
	id := c.next
	if params == nil {
		params = []any{}
	}
	b, err := msgpack.Marshal([]any{int64(0), id, method, params})
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	v, err := msgpack.Decode(c.br, nil)
	if err != nil {
		c.t.Fatalf("decode: %v", err)
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 4 || arr[0] != int64(1) {
		c.t.Fatalf("response shape: %#v", v)
	}
	if got, _ := arr[1].(int64); got != id {
		c.t.Fatalf("msgid = %v, want %d", arr[1], id)
	}
	return arr[2], arr[3]
}

// hello performs a compatible handshake.
func (c *client) hello() {
	c.t.Helper()
	errVal, result := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol, "name": "test"})
	if errVal != nil {
		c.t.Fatalf("hello err: %#v", errVal)
	}
	m := result.(map[string]any)
	if m["server"] != "autodb" || m["protocol"] != rpc.Protocol {
		c.t.Fatalf("hello reply: %#v", m)
	}
}

// login performs the auth.login round-trip and returns the wire token.
func (c *client) login(name, pass string) string {
	c.t.Helper()
	errVal, result := c.call("auth.login", name, pass)
	if errVal != nil {
		c.t.Fatalf("login %s: %#v", name, errVal)
	}
	token, _ := result.(map[string]any)["token"].(string)
	if token == "" {
		c.t.Fatalf("login %s result carried no token: %#v", name, result)
	}
	return token
}

// mustErr asserts a wire error with the given code and returns its message.
func mustErr(t *testing.T, errVal any, code int64) string {
	t.Helper()
	m, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("wire error shape: %#v", errVal)
	}
	if got, _ := m["code"].(int64); got != code {
		t.Fatalf("error code = %v, want %d (%#v)", m["code"], code, m)
	}
	msg, _ := m["message"].(string)
	return msg
}

// auditCount reports how many audit rows the wire path left under action.
func (f *fixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	n, err := f.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("counting %q audit rows: %v", action, err)
	}
	return int(n)
}

// audits returns the audit rows for action, for detail assertions.
func (f *fixture) audits(t *testing.T, action string) []*meta.AuditEntry {
	t.Helper()
	rows, err := f.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Select()
	if err != nil {
		t.Fatalf("reading %q audit rows: %v", action, err)
	}
	return rows
}
