package rpc_test

import (
	"bufio"
	"context"
	"errors"
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
	rootTok string
	connID  int64
	addr    string
}

var fixtureSeq atomic.Int64

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

	// A declared protocol of -1 must poison like any other mismatch — it
	// must never collide with the omitted-protocol probe form (lector r2).
	cNeg := f.dial(t)
	errVal, _ := cNeg.call("sys.hello", map[string]any{"protocol": int64(-1)})
	mustErr(t, errVal, rpc.CodeProtocolMismatch)
	errVal, _ = cNeg.call("auth.needs_bootstrap")
	mustErr(t, errVal, rpc.CodeProtocolMismatch)

	c := f.dial(t)
	errVal, _ = c.call("sys.hello", map[string]any{"protocol": int64(999)})
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

func TestShutdownWithLiveConnection(t *testing.T) {
	t.Parallel()
	// A dedicated fixture whose server we shut down under a live connection.
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

	// A request completed before shutdown proves the connection is live;
	// then shutdown with the connection still open must drain cleanly
	// (Run returns nil) and close the socket. Racing a request AGAINST
	// shutdown is deliberately not asserted here: under drain-safe
	// admission a message decoded after drain begins is refused by
	// design, so the outcome is timing-dependent — the drain-window
	// flush property is proven deterministically in golib's transport
	// suite, where a test handler synchronizes on entry.
	errVal, result := c.call("auth.whoami", rootTok)
	if errVal != nil {
		t.Fatalf("whoami: %#v", errVal)
	}
	if u := result.(map[string]any); u["name"] != "root" {
		t.Fatalf("whoami: %#v", u)
	}

	cancel()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.br.ReadByte(); err == nil {
		t.Fatal("connection still open after shutdown")
	}
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --- review-fold regression tests (2026-08-16 lector autodb-M5 r1) ---

// fakeOccupant answers every accepted connection with a fixed raw frame.
func fakeOccupant(t *testing.T, frame []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write(frame)
			conn.Close()
		}
	}()
	return ln.Addr().String()
}

// Must-fix 1: Probe authenticates the full response frame — a foreign
// occupant sending a plausible-but-wrong frame must never make the
// single-instance guard report "already running".
func TestProbeRejectsForgedFrames(t *testing.T) {
	t.Parallel()
	goodResult := map[string]any{"server": "autodb", "protocol": rpc.Protocol, "version": "v"}
	cases := []struct {
		name  string
		frame any
	}{
		{"request-shaped frame", []any{int64(0), int64(1), "sys.hello", []any{goodResult}}},
		{"notification frame", []any{int64(2), "sys.hello", []any{goodResult}}},
		{"wrong msgid", []any{int64(1), int64(99), nil, goodResult}},
		{"error response", []any{int64(1), int64(1), "boom", nil}},
		{"error and plausible result", []any{int64(1), int64(1), "boom", goodResult}},
		{"missing version", []any{int64(1), int64(1), nil,
			map[string]any{"server": "autodb", "protocol": rpc.Protocol}}},
		{"wrong server name", []any{int64(1), int64(1), nil,
			map[string]any{"server": "otherdb", "protocol": rpc.Protocol, "version": "v"}}},
		{"wrong protocol", []any{int64(1), int64(1), nil,
			map[string]any{"server": "autodb", "protocol": int64(99), "version": "v"}}},
		{"result not a map", []any{int64(1), int64(1), nil, "autodb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Per-subtest ctx: a parent ctx's deferred cancel fires before
			// parallel subtests execute.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			frame, err := msgpack.Marshal(tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			addr := fakeOccupant(t, frame)
			if _, err := rpc.Probe(ctx, addr); !errors.Is(err, rpc.ErrNotAutodb) {
				t.Fatalf("err = %v, want ErrNotAutodb", err)
			}
		})
	}
}

// Must-fix 2: the incompatible-hello audit row is a durable promise — when
// it cannot persist, the failure surfaces (generic internal error, R6)
// instead of being swallowed, and the session stays poisoned.
func TestIncompatibleHelloAuditFailureSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1"); err != nil {
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
	defer func() {
		cancel()
		<-errc
	}()
	for srv.Addr() == "" {
		time.Sleep(time.Millisecond)
	}
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &client{t: t, conn: conn, br: bufio.NewReader(conn)}

	// Kill the audit store, then send an incompatible hello: the peer must
	// see a generic internal error (audit could not persist), not the
	// protocol-mismatch code the promise couldn't record.
	_ = store.Close()
	errVal, _ := c.call("sys.hello", map[string]any{"protocol": int64(999)})
	mustErr(t, errVal, -32603)

	// The session is poisoned regardless.
	errVal, _ = c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	mustErr(t, errVal, rpc.CodeProtocolMismatch)
}

// Must-fix 4: an IPv6 bind must produce a bracketed, bindable address.
func TestIPv6Bind(t *testing.T) {
	t.Parallel()
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable:", err)
	}
	probe.Close()

	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	srv := rpc.New(svc, eng, config.Server{Bind: "::1", Port: 0}, "test")
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	deadline := time.After(2 * time.Second)
	for srv.Addr() == "" {
		select {
		case <-deadline:
			t.Fatal("IPv6 server never bound")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pcancel()
	if ver, err := rpc.Probe(pctx, srv.Addr()); err != nil || ver != "test" {
		t.Fatalf("Probe over IPv6 = %q, %v", ver, err)
	}
}

// Should-fix 1: a present-but-non-integer hello protocol is an invalid
// call — neither a probe nor a poisoning — and exact arity is enforced.
func TestStrictParams(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)

	// Malformed protocol declaration: invalid params, session stays clean.
	errVal, _ := c.call("sys.hello", map[string]any{"protocol": "one"})
	mustErr(t, errVal, -32602)
	c.hello() // still admissible

	// Trailing extra argument: refused, not silently ignored.
	errVal, _ = c.call("auth.whoami", f.rootTok, "extra")
	mustErr(t, errVal, -32602)

	// Extra hello argument likewise.
	errVal, _ = c.call("sys.hello", map[string]any{}, "extra")
	mustErr(t, errVal, -32602)
}

// --- M6 surface: schema.*, workspace.*, protocol 2 (ADR-0057 §6/§7) ---

func TestHelloCarriesInstanceAndProtocol(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	errVal, result := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol})
	if errVal != nil {
		t.Fatalf("hello: %#v", errVal)
	}
	m := result.(map[string]any)
	// Track the constant: the hello reply must advertise whatever the
	// server actually speaks, so bumping Protocol cannot silently skip
	// the handshake contract.
	if m["protocol"] != rpc.Protocol {
		t.Fatalf("protocol = %v, want %d", m["protocol"], rpc.Protocol)
	}
	inst, _ := m["instance"].(string)
	if len(inst) != 16 {
		t.Fatalf("instance = %q, want 16 hex chars", inst)
	}
}

func TestSchemaIntrospectionOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// Seed a table through the engine.
	if errVal, _ := c.call("exec.run", f.rootTok, f.connID,
		"CREATE TABLE tracks (id INTEGER PRIMARY KEY, name TEXT NOT NULL)"); errVal != nil {
		t.Fatalf("create: %#v", errVal)
	}

	errVal, result := c.call("schema.tables", f.rootTok, f.connID, "")
	if errVal != nil {
		t.Fatalf("schema.tables: %#v", errVal)
	}
	var found map[string]any
	for _, row := range result.([]any) {
		m := row.(map[string]any)
		if m["name"] == "tracks" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("tracks not listed: %#v", result)
	}
	if found["kind"] != "table" || found["quoted"] == "" || found["quoted"] == nil {
		t.Fatalf("table row = %#v (server-quoted name required)", found)
	}

	// The server-quoted identifier drives the quick-select scaffold.
	quoted := found["quoted"].(string)
	if errVal, _ = c.call("exec.run", f.rootTok, f.connID,
		"SELECT * FROM "+quoted+" LIMIT 100"); errVal != nil {
		t.Fatalf("scaffold select over quoted name: %#v", errVal)
	}

	errVal, result = c.call("schema.columns", f.rootTok, f.connID, "", "tracks")
	if errVal != nil {
		t.Fatalf("schema.columns: %#v", errVal)
	}
	cols := result.([]any)
	if len(cols) != 2 {
		t.Fatalf("columns = %#v", cols)
	}
	first := cols[0].(map[string]any)
	if first["name"] != "id" || first["pk"] != true {
		t.Fatalf("column 0 = %#v", first)
	}

	// sqlite has no stored routines: capability absence is DATA.
	errVal, result = c.call("schema.routines", f.rootTok, f.connID, "")
	if errVal != nil {
		t.Fatalf("schema.routines: %#v", errVal)
	}
	r := result.(map[string]any)
	if r["supported"] != false {
		t.Fatalf("routines = %#v, want supported=false for sqlite", r)
	}

	// R13: a user with no grant on the connection learns nothing.
	if errVal, _ := c.call("auth.user_create", f.rootTok, "nogrant", "nogrant-pass", "reader"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, result = c.call("auth.login", "nogrant", "nogrant-pass")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	tok := result.(map[string]any)["token"].(string)
	errVal, _ = c.call("schema.tables", tok, f.connID, "")
	mustErr(t, errVal, rpc.CodeDenied)
}

func TestWorkspacesOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("workspace.create", f.rootTok, "analytics")
	if errVal != nil {
		t.Fatalf("workspace.create: %#v", errVal)
	}
	wsID := result.(int64)
	if errVal, _ = c.call("workspace.attach", f.rootTok, wsID, f.connID); errVal != nil {
		t.Fatalf("attach: %#v", errVal)
	}

	// Admin sees the workspace with its connection.
	errVal, result = c.call("workspace.list", f.rootTok)
	if errVal != nil {
		t.Fatalf("list: %#v", errVal)
	}
	views := result.([]any)
	if len(views) != 1 {
		t.Fatalf("views = %#v", views)
	}
	v := views[0].(map[string]any)
	if v["name"] != "analytics" || len(v["connections"].([]any)) != 1 {
		t.Fatalf("view = %#v", v)
	}

	// A reader with NO grant: the workspace is OMITTED entirely (R13).
	if errVal, _ = c.call("auth.user_create", f.rootTok, "wsreader", "wsreader-pass", "reader"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, result = c.call("auth.login", "wsreader", "wsreader-pass")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	readerTok := result.(map[string]any)["token"].(string)
	readerID := result.(map[string]any)["user"].(map[string]any)["id"].(int64)

	errVal, result = c.call("workspace.list", readerTok)
	if errVal != nil {
		t.Fatalf("reader list: %#v", errVal)
	}
	if got := len(result.([]any)); got != 0 {
		t.Fatalf("ungranted reader sees %d workspaces, want 0", got)
	}

	// Granted reader sees it, with only the granted connection.
	if errVal, _ = c.call("auth.grant_add", f.rootTok, readerID, f.connID, "reader"); errVal != nil {
		t.Fatalf("grant_add: %#v", errVal)
	}
	errVal, result = c.call("workspace.list", readerTok)
	if errVal != nil {
		t.Fatalf("granted list: %#v", errVal)
	}
	if got := len(result.([]any)); got != 1 {
		t.Fatalf("granted reader sees %d workspaces, want 1", got)
	}

	// Writes are admin-gated.
	errVal, _ = c.call("workspace.create", readerTok, "sneaky")
	mustErr(t, errVal, rpc.CodeDenied)

	// Rename + detach + delete round out the lifecycle.
	if errVal, _ = c.call("workspace.rename", f.rootTok, wsID, "renamed"); errVal != nil {
		t.Fatalf("rename: %#v", errVal)
	}
	if errVal, _ = c.call("workspace.detach", f.rootTok, wsID, f.connID); errVal != nil {
		t.Fatalf("detach: %#v", errVal)
	}
	if errVal, _ = c.call("workspace.delete", f.rootTok, wsID); errVal != nil {
		t.Fatalf("delete: %#v", errVal)
	}
	errVal, _ = c.call("workspace.rename", f.rootTok, wsID, "ghost")
	mustErr(t, errVal, -32602) // not-found surfaces as the mapped sentinel
}

// sys.shutdown is the supported restart path for the shared server
// (ADR-0056 §3): admin-only, and the reply is delivered before the
// listener closes.
func TestShutdownOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// A non-admin is refused.
	if errVal, _ := c.call("auth.user_create", f.rootTok, "peon", "peon-passphrase", "editor"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, result := c.call("auth.login", "peon", "peon-passphrase")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	peonTok := result.(map[string]any)["token"].(string)
	errVal, _ = c.call("sys.shutdown", peonTok)
	mustErr(t, errVal, rpc.CodeDenied)

	// An admin gets an acknowledged shutdown — the reply arrives, which
	// is the drain property that matters.
	errVal, result = c.call("sys.shutdown", f.rootTok)
	if errVal != nil {
		t.Fatalf("admin shutdown: %#v", errVal)
	}
	if m, ok := result.(map[string]any); !ok || m["stopping"] != true {
		t.Fatalf("shutdown reply = %#v, want stopping:true", result)
	}
}
