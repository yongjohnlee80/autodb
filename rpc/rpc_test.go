package rpc_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
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
	rootTok string
	connID  int64
	addr    string
}

var fixtureSeq int64

func newFixture(t *testing.T) *fixture {
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

	fixtureSeq++
	dsn := fmt.Sprintf("file:rpctest%d_%d?mode=memory&cache=shared", time.Now().UnixNano(), fixtureSeq)
	connID, err := eng.CreateConnection(ctx, rootTok, "target", "sqlite", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version",
		rpc.WithListener(ln))
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
	return &fixture{rootTok: rootTok, connID: connID, addr: srv.Addr()}
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

func TestHandshakeGate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)

	// Any method before hello is refused without touching the core.
	errVal, _ := c.call("auth.needs_bootstrap")
	mustErr(t, errVal, rpc.CodeHandshakeRequired)

	// A probe hello (no protocol) answers but does NOT admit.
	errVal, result := c.call("sys.hello", map[string]any{})
	if errVal != nil {
		t.Fatalf("probe hello err: %#v", errVal)
	}
	if m := result.(map[string]any); m["protocol"] != rpc.Protocol {
		t.Fatalf("probe reply: %#v", m)
	}
	errVal, _ = c.call("auth.needs_bootstrap")
	mustErr(t, errVal, rpc.CodeHandshakeRequired)

	// A compatible hello admits the session.
	c.hello()
	errVal, result = c.call("auth.needs_bootstrap")
	if errVal != nil {
		t.Fatalf("post-hello err: %#v", errVal)
	}
	if result != false { // fixture is bootstrapped
		t.Fatalf("needs_bootstrap = %#v", result)
	}
}

func TestHandshakeIncompatiblePoisonsSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)

	errVal, _ := c.call("sys.hello", map[string]any{"protocol": int64(999)})
	mustErr(t, errVal, rpc.CodeProtocolMismatch)

	// Everything afterward is refused — including a now-compatible hello:
	// the client must reconnect with a compatible binary.
	errVal, _ = c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	mustErr(t, errVal, rpc.CodeProtocolMismatch)
	errVal, _ = c.call("auth.needs_bootstrap")
	mustErr(t, errVal, rpc.CodeProtocolMismatch)

	// A fresh connection is unaffected.
	c2 := f.dial(t)
	c2.hello()
}

func TestAuthFlowOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// Login with the bootstrapped account.
	errVal, result := c.call("auth.login", "root", "root-passphrase")
	if errVal != nil {
		t.Fatalf("login err: %#v", errVal)
	}
	login := result.(map[string]any)
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("login result: %#v", login)
	}
	user := login["user"].(map[string]any)
	if user["name"] != "root" || user["role"] != "admin" {
		t.Fatalf("login user: %#v", user)
	}

	errVal, result = c.call("auth.whoami", token)
	if errVal != nil {
		t.Fatalf("whoami err: %#v", errVal)
	}
	if u := result.(map[string]any); u["name"] != "root" {
		t.Fatalf("whoami: %#v", u)
	}

	// Bad credentials surface as CodeAuth without internals.
	errVal, _ = c.call("auth.login", "root", "wrong-passphrase")
	mustErr(t, errVal, rpc.CodeAuth)

	// A garbage token likewise.
	errVal, _ = c.call("auth.whoami", "not-a-token")
	mustErr(t, errVal, rpc.CodeAuth)

	// Logout invalidates the session.
	if errVal, _ := c.call("auth.logout", token); errVal != nil {
		t.Fatalf("logout err: %#v", errVal)
	}
	errVal, _ = c.call("auth.whoami", token)
	mustErr(t, errVal, rpc.CodeAuth)
}

func TestExecOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	run := func(sql string) (any, any) {
		return c.call("exec.run", f.rootTok, f.connID, sql)
	}

	if errVal, _ := run("CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"); errVal != nil {
		t.Fatalf("create: %#v", errVal)
	}
	if errVal, _ := run("INSERT INTO songs (title) VALUES ('one'), ('two')"); errVal != nil {
		t.Fatalf("insert: %#v", errVal)
	}

	errVal, result := run("SELECT id, title FROM songs ORDER BY id")
	if errVal != nil {
		t.Fatalf("select: %#v", errVal)
	}
	res := result.(map[string]any)
	if res["class"] != "read" || res["more"] != false {
		t.Fatalf("select result meta: %#v", res)
	}
	cols := res["columns"].([]any)
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "title" {
		t.Fatalf("columns: %#v", cols)
	}
	rows := res["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows: %#v", rows)
	}
	first := rows[0].([]any)
	if first[0] != int64(1) || first[1] != "one" {
		t.Fatalf("row 0: %#v", first)
	}

	// The WHERE-less guard crosses the wire as a statement rejection.
	errVal, _ = run("UPDATE songs SET title = 'x'")
	mustErr(t, errVal, rpc.CodeStatementRejected)

	// Reader authorization is enforced through the projection: a reader can
	// SELECT but not UPDATE.
	errVal, result = c.call("auth.user_create", f.rootTok, "reader1", "reader-pass", "reader")
	if errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	readerID := result.(int64)
	if errVal, _ = c.call("auth.grant_add", f.rootTok, readerID, f.connID, "reader"); errVal != nil {
		t.Fatalf("grant_add: %#v", errVal)
	}
	errVal, result = c.call("auth.login", "reader1", "reader-pass")
	if errVal != nil {
		t.Fatalf("reader login: %#v", errVal)
	}
	readerTok := result.(map[string]any)["token"].(string)

	if errVal, _ := c.call("exec.run", readerTok, f.connID, "SELECT count(*) FROM songs"); errVal != nil {
		t.Fatalf("reader select: %#v", errVal)
	}
	errVal, _ = c.call("exec.run", readerTok, f.connID, "UPDATE songs SET title = 'x' WHERE id = 1")
	mustErr(t, errVal, rpc.CodeDenied)
}

func TestConnManagementOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("conn.list", f.rootTok)
	if errVal != nil {
		t.Fatalf("conn.list: %#v", errVal)
	}
	list := result.([]any)
	if len(list) != 1 {
		t.Fatalf("conn.list: %#v", list)
	}
	row := list[0].(map[string]any)
	if row["name"] != "target" || row["engine"] != "sqlite" || row["id"] != f.connID {
		t.Fatalf("conn row: %#v", row)
	}

	if errVal, _ := c.call("conn.test", f.rootTok, f.connID); errVal != nil {
		t.Fatalf("conn.test: %#v", errVal)
	}

	// Invalid params answer CodeInvalidParams (-32602), connection survives.
	errVal, _ = c.call("conn.test", f.rootTok, "not-an-int")
	mustErr(t, errVal, -32602)
	if errVal, _ := c.call("conn.test", f.rootTok, f.connID); errVal != nil {
		t.Fatalf("post-invalid conn.test: %#v", errVal)
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A live compatible server reports its version.
	ver, err := rpc.Probe(ctx, f.addr)
	if err != nil || ver != "test-version" {
		t.Fatalf("Probe = %q, %v", ver, err)
	}

	// Nothing listening → the dial error (the FE spawn signal).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	if _, err := rpc.Probe(ctx, dead); err == nil || errors.Is(err, rpc.ErrNotAutodb) {
		t.Fatalf("dead port: err = %v, want plain dial error", err)
	}

	// A foreign occupant → ErrNotAutodb.
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	go func() {
		for {
			conn, err := foreign.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			conn.Close()
		}
	}()
	if _, err := rpc.Probe(ctx, foreign.Addr().String()); !errors.Is(err, rpc.ErrNotAutodb) {
		t.Fatalf("foreign occupant: err = %v, want ErrNotAutodb", err)
	}
}

func TestGracefulDrainFlushesInflightReply(t *testing.T) {
	t.Parallel()
	// A dedicated fixture whose server we stop mid-request.
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	defer eng.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test", rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	for srv.Addr() == "" {
		time.Sleep(time.Millisecond)
	}

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &client{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.hello()

	// Fire a request, then immediately begin shutdown. The polite-drain
	// property is that a WELL-FORMED reply with msgid fidelity arrives
	// instead of a dropped connection; whether it carries the result or a
	// cancellation-driven error is inherently racy (Shutdown cancels
	// handler contexts by design), so only the frame shape is asserted.
	b, _ := msgpack.Marshal([]any{int64(0), int64(42), "auth.whoami", []any{rootTok}})
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	cancel()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	v, err := msgpack.Decode(c.br, nil)
	if err != nil {
		t.Fatalf("drained reply never arrived: %v", err)
	}
	arr := v.([]any)
	if len(arr) != 4 || arr[0] != int64(1) || arr[1] != int64(42) {
		t.Fatalf("drained reply: %#v", arr)
	}
	if arr[2] == nil {
		if u, ok := arr[3].(map[string]any); !ok || u["name"] != "root" {
			t.Fatalf("drained result: %#v", arr[3])
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
