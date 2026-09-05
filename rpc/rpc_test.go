package rpc_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/msgpack"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// The fixture (newFixture, session, dial, call, hello, login, mustErr,
// auditCount, audits) lives in fixture_test.go — the §11 entry-point
// fixture for this package.

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

// (Example §11 conversion: dial+hello became f.session, the reader login
// dance became c.login, and the audit promise is checked with auditCount.)
func TestExecOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.session(t)

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
	readerTok := c.login("reader1", "reader-pass")

	if errVal, _ := c.call("exec.run", readerTok, f.connID, "SELECT count(*) FROM songs"); errVal != nil {
		t.Fatalf("reader select: %#v", errVal)
	}
	errVal, _ = c.call("exec.run", readerTok, f.connID, "UPDATE songs SET title = 'x' WHERE id = 1")
	mustErr(t, errVal, rpc.CodeDenied)

	// The wire path keeps the engine's audit promise: attempts and the
	// denial above all left rows.
	if f.auditCount(t, "exec") == 0 {
		t.Error("no exec audit rows after exec.run over the wire")
	}
	if f.auditCount(t, "exec_rejected") == 0 {
		t.Error("no exec_rejected audit rows after a denied exec.run")
	}
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
	// The pid must be THIS process (frontends use it to identify which
	// backend they are driving), and the address must be the live one.
	if got := m["pid"]; got != int64(os.Getpid()) {
		t.Fatalf("pid = %v, want %d", got, os.Getpid())
	}
	if addr, _ := m["addr"].(string); addr == "" {
		t.Fatal("hello reported no listen address")
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

// A denied statement must come back as a structured error the frontends
// can render — a reader running DELETE is the case that looked silent in
// M6 manual testing.
func TestDeniedStatementReportsOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	if errVal, _ := c.call("exec.run", f.rootTok, f.connID,
		"CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"); errVal != nil {
		t.Fatalf("scaffold: %#v", errVal)
	}
	if errVal, _ := c.call("auth.user_create", f.rootTok, "onlyreader", "reader-passphrase", "reader"); errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, result := c.call("auth.login", "onlyreader", "reader-passphrase")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	tok := result.(map[string]any)["token"].(string)

	// No grant at all: denied without disclosing the connection.
	errVal, _ = c.call("exec.run_script", tok, f.connID, "DELETE FROM songs WHERE id = 1")
	mustErr(t, errVal, rpc.CodeDenied)

	// With a reader grant: SELECT runs, DELETE is still denied — and the
	// denial carries a message, not an empty error.
	if errVal, _ := c.call("auth.grant_add", f.rootTok, result.(map[string]any)["user"].(map[string]any)["id"], f.connID, "reader"); errVal != nil {
		t.Fatalf("grant_add: %#v", errVal)
	}
	if errVal, _ := c.call("exec.run_script", tok, f.connID, "SELECT count(*) FROM songs"); errVal != nil {
		t.Fatalf("reader select: %#v", errVal)
	}
	errVal, _ = c.call("exec.run_script", tok, f.connID, "DELETE FROM songs WHERE id = 1")
	mustErr(t, errVal, rpc.CodeDenied)
	if msg, _ := errVal.(map[string]any)["message"].(string); msg == "" {
		t.Fatalf("denial carried no message: %#v", errVal)
	}
}

// A multi-statement script runs every statement and returns the last
// result; a failure mid-script says how many already ran.
func TestRunScriptOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	if errVal, _ := c.call("exec.run", f.rootTok, f.connID,
		"CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"); errVal != nil {
		t.Fatalf("scaffold: %#v", errVal)
	}
	errVal, result := c.call("exec.run_script", f.rootTok, f.connID,
		"INSERT INTO songs (title) VALUES ('one'); SELECT title FROM songs ORDER BY id DESC")
	if errVal != nil {
		t.Fatalf("run_script: %#v", errVal)
	}
	m := result.(map[string]any)
	if m["statements"] != int64(2) {
		t.Fatalf("statements = %v, want 2", m["statements"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result from the last statement: %#v", m)
	}
	if len(res["rows"].([]any)) == 0 {
		t.Fatalf("last statement returned no rows: %#v", res)
	}

	// A failure stops the script and reports the partial application.
	errVal, _ = c.call("exec.run_script", f.rootTok, f.connID,
		"INSERT INTO songs (title) VALUES ('two'); UPDATE songs SET title = 'x'")
	mustErr(t, errVal, rpc.CodeStatementRejected)
}

// TestLoginOverUnixSocket reproduces the field bug end to end: the daemon's
// default transport is a unix socket, whose peer has no IP, so every login
// after bootstrap was refused "ip not allowed". Bootstrap worked because it
// does not consult the allowlist; the second connection's auth.login did.
func TestLoginOverUnixSocket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Loopback-only allowlist — the shipped default. No IP a socket could
	// present is in it.
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", auth.LocalPeer); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	sock := t.TempDir() + "/adb.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := rpc.New(svc, eng, config.Server{}, "test-version", rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(ctx)
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

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &client{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.hello()

	// This is the call that failed in the field.
	errVal, result := c.call("auth.login", "root", "root-passphrase")
	if errVal != nil {
		t.Fatalf("auth.login over unix socket errored: %#v", errVal)
	}
	m, ok := result.(map[string]any)
	if !ok || m["token"] == nil || m["token"] == "" {
		t.Fatalf("auth.login over unix socket returned no token: %#v", result)
	}
}

// TestHelloReportsNotesDir: the handshake carries the notes root so a
// frontend lists the right per-workspace folders without re-deriving
// config (WithNotesDir → sys.hello "notes_dir").
func TestHelloReportsNotesDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version",
		rpc.WithListener(ln), rpc.WithNotesDir("/tmp/autodb-notes-xyz"))
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-errc })
	deadline := time.After(2 * time.Second)
	for srv.Addr() == "" {
		select {
		case <-deadline:
			t.Fatal("server never bound")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &client{t: t, conn: conn, br: bufio.NewReader(conn)}
	_, result := c.call("sys.hello", map[string]any{"protocol": rpc.Protocol, "name": "test"})
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("hello reply not a map: %#v", result)
	}
	if m["notes_dir"] != "/tmp/autodb-notes-xyz" {
		t.Fatalf("hello notes_dir = %#v, want /tmp/autodb-notes-xyz", m["notes_dir"])
	}
}

func TestUserIPAllowlistOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("auth.login", "root", "root-passphrase")
	if errVal != nil {
		t.Fatalf("login err: %#v", errVal)
	}
	login := result.(map[string]any)
	token, _ := login["token"].(string)
	rootID, _ := login["user"].(map[string]any)["id"].(int64)

	// The empty-cidr self-service gesture: the SERVER substitutes the
	// address this session connects from (loopback in the fixture) — the
	// one thing the rpc layer itself implements for this surface.
	if errVal, _ := c.call("auth.user_allowlist_add", token, rootID, "", "this machine"); errVal != nil {
		t.Fatalf("user_allowlist_add(empty cidr) err: %#v", errVal)
	}
	errVal, res := c.call("auth.user_allowlist_list", token, rootID)
	if errVal != nil {
		t.Fatalf("user_allowlist_list err: %#v", errVal)
	}
	rows := res.([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	cidr, _ := row["cidr"].(string)
	if cidr != "127.0.0.1/32" && cidr != "::1/128" {
		t.Fatalf("cidr = %q, want the fixture peer address as a single-address prefix", cidr)
	}
	if row["label"] != "this machine" {
		t.Fatalf("label = %#v", row["label"])
	}

	// The global list verb round-trips config + store split.
	errVal, res = c.call("auth.allowlist_list", token)
	if errVal != nil {
		t.Fatalf("allowlist_list err: %#v", errVal)
	}
	gl := res.(map[string]any)
	if len(gl["config"].([]any)) == 0 {
		t.Fatal("allowlist_list returned no config CIDRs — fixture seeds loopback")
	}

	// Removal round-trips and the list empties.
	rowID, _ := row["id"].(int64)
	if errVal, _ := c.call("auth.user_allowlist_remove", token, rootID, rowID); errVal != nil {
		t.Fatalf("user_allowlist_remove err: %#v", errVal)
	}
	errVal, res = c.call("auth.user_allowlist_list", token, rootID)
	if errVal != nil {
		t.Fatalf("relist err: %#v", errVal)
	}
	if n := len(res.([]any)); n != 0 {
		t.Fatalf("rows after remove = %d, want 0", n)
	}
}

// The ExecSession verbs over the wire (R5). Session SEMANTICS — pinning,
// transaction state, atomicity — are core's, tested against a live
// PostgreSQL; what belongs here is that the verbs exist, validate their
// arguments, carry the session id, and project results like every other
// exec verb.
func TestSessionVerbsOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	if errVal, _ := c.call("exec.run", f.rootTok, f.connID,
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)"); errVal != nil {
		t.Fatalf("scaffold: %#v", errVal)
	}

	errVal, result := c.call("exec.session_open", f.rootTok, f.connID)
	if errVal != nil {
		t.Fatalf("session_open: %#v", errVal)
	}
	sid, _ := result.(map[string]any)["session_id"].(string)
	if sid == "" {
		t.Fatalf("session_open returned no session id: %#v", result)
	}

	errVal, result = c.call("exec.session_run", f.rootTok, sid, "SELECT 1")
	if errVal != nil {
		t.Fatalf("session_run: %#v", errVal)
	}
	if rows, ok := result.(map[string]any)["rows"].([]any); !ok || len(rows) != 1 {
		t.Fatalf("session_run did not project a result: %#v", result)
	}

	errVal, result = c.call("exec.session_close", f.rootTok, sid)
	if errVal != nil {
		t.Fatalf("session_close: %#v", errVal)
	}
	if result.(map[string]any)["closed"] != true {
		t.Fatalf("session_close reply: %#v", result)
	}

	// A closed session is gone, and a made-up id is refused identically —
	// the id space must not be usable to discover which sessions exist.
	errVal, _ = c.call("exec.session_run", f.rootTok, sid, "SELECT 1")
	if errVal == nil {
		t.Fatal("a closed session still ran a statement")
	}
	bogus, _ := c.call("exec.session_run", f.rootTok, "nope-not-a-session", "SELECT 1")
	if bogus == nil {
		t.Fatal("an invented session id ran a statement")
	}
	if fmt.Sprintf("%#v", errVal) != fmt.Sprintf("%#v", bogus) {
		t.Errorf("a closed session and an invented one are distinguishable over the wire "+
			"(%#v vs %#v); the difference tells a caller which ids existed", errVal, bogus)
	}

	// Arity is enforced, like every other verb.
	for _, bad := range [][]any{
		{"exec.session_open", f.rootTok},
		{"exec.session_open", f.rootTok, f.connID, "extra"},
		{"exec.session_close", f.rootTok},
		{"exec.session_run", f.rootTok, sid},
	} {
		method := bad[0].(string)
		if errVal, _ := c.call(method, bad[1:]...); errVal == nil {
			t.Errorf("%s accepted %d arguments", method, len(bad)-1)
		}
	}
}

// The protocol bump is the point of the bump: a protocol-4 client sending
// `BEGIN; …; COMMIT;` to exec.run_script got independent statements, and the
// same text now runs in one transaction. Same verb, different meaning — so a
// stale client must be stopped at the handshake rather than surprised by it.
func TestProtocolBumpRefusesTheOlderClient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	if rpc.Protocol != 5 {
		t.Fatalf("Protocol = %d, want 5 — R5 changed what exec.run_script MEANS for a "+
			"transactional script, and an unbumped handshake lets a client keep the old "+
			"assumption against the new behaviour", rpc.Protocol)
	}
	c := f.dial(t)
	errVal, _ := c.call("sys.hello", map[string]any{"protocol": int64(4), "name": "stale"})
	if errVal == nil {
		t.Fatal("a protocol-4 client completed the handshake against a protocol-5 server")
	}
}

// tx.status over the wire (protocol 5, ADR-0074 Amendment 4 A2). R5 owns this
// verb; R4 owns the outcome machine underneath it.
func TestTxStatusOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// An id with no record is NOT pending. A mistyped or expired id must not
	// leave a caller polling forever for a transaction that never existed.
	errVal, _ := c.call("tx.status", f.rootTok, "tx-that-never-was")
	mustErr(t, errVal, rpc.CodeNoSuchTx)

	// The pending list answers even when nothing is pending, and answers
	// with a LIST rather than an error — "nothing is stuck" is a real answer.
	errVal, result := c.call("tx.status", f.rootTok, "", int64(10))
	if errVal != nil {
		t.Fatalf("tx.status pending: %#v", errVal)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("pending reply shape: %#v", result)
	}
	if _, ok := m["pending"].([]any); !ok {
		t.Fatalf("pending is not a list: %#v", m)
	}

	// Arity: two DEFINED forms, and nothing else.
	//
	// The third case is the one lector found. The handler accepted three
	// arguments globally and then, seeing a non-empty id, answered about the
	// transaction — silently discarding the limit. `tx.status(token, id,
	// limit)` is a reading that looks obvious and means nothing here, and a
	// verb that ignores an argument teaches the wrong contract to whoever
	// copies the call.
	for _, tc := range []struct {
		name string
		args []any
	}{
		{"too few", []any{f.rootTok}},
		{"too many", []any{f.rootTok, "", int64(1), "extra"}},
		{"a limit alongside a tx id is not a defined form",
			[]any{f.rootTok, "tx-that-never-was", int64(1)}},
	} {
		errVal, _ := c.call("tx.status", tc.args...)
		if errVal == nil {
			t.Errorf("tx.status accepted %s: %v", tc.name, tc.args)
			continue
		}
		m, _ := errVal.(map[string]any)
		if code, _ := m["code"].(int64); code != golibrpc.CodeInvalidParams {
			t.Errorf("%s: code = %d, want CodeInvalidParams (%d) — an undefined shape must be "+
				"refused as malformed, not answered as if it were one of the defined ones",
				tc.name, code, golibrpc.CodeInvalidParams)
		}
	}

	// The verb is unreachable before a handshake, like every other method.
	fresh := f.dial(t)
	if errVal, _ := fresh.call("tx.status", f.rootTok, "x"); errVal == nil {
		t.Error("tx.status answered before sys.hello")
	}
}

// PAT management over the wire (ADR-0075 §4). The credential itself is the
// front door's business; this is the surface a person uses to create and
// revoke one from the tools they already have.
func TestTokenVerbsOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("auth.token_create", f.rootTok, "laptop", int64(0), "", f.frontDoorConn(t), int64(0))
	if errVal != nil {
		t.Fatalf("token_create: %#v", errVal)
	}
	m := result.(map[string]any)
	secret, _ := m["secret"].(string)
	if secret == "" {
		t.Fatal("token_create returned no secret; the reply is the only time it exists")
	}
	if !strings.HasPrefix(secret, "adb_pat_") {
		t.Errorf("secret %q lacks the scannable prefix", secret)
	}

	// The list never carries the credential — not the digest, and not the
	// selector, which is half of it. Publishing the selector would turn a
	// token list into a head start.
	errVal, result = c.call("auth.token_list", f.rootTok, int64(0))
	if errVal != nil {
		t.Fatalf("token_list: %#v", errVal)
	}
	rows := result.([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["name"] != "laptop" {
		t.Errorf("name = %v", row["name"])
	}
	if row["last_used"] != "never" {
		t.Errorf("last_used = %v, want \"never\" — a zero timestamp rendered as 1970 puts a "+
			"plausible date on a token nobody has used, which is the row an operator is "+
			"scanning for", row["last_used"])
	}
	for k, v := range row {
		if sv, ok := v.(string); ok && sv != "" && strings.Contains(secret, sv) && len(sv) > 8 {
			t.Errorf("the listing field %q carries part of the credential", k)
		}
	}

	// A duplicate name is refused, and the reason is NAMED: this caller is
	// authenticated and managing their own tokens, unlike the anonymous
	// front-door path whose failure is deliberately uniform.
	errVal, _ = c.call("auth.token_create", f.rootTok, "laptop", int64(0), "", f.frontDoorConn(t), int64(0))
	mustErr(t, errVal, rpc.CodeInvalidToken)

	// An over-long lifetime is refused too.
	errVal, _ = c.call("auth.token_create", f.rootTok, "forever", int64(400), "", f.frontDoorConn(t), int64(0))
	mustErr(t, errVal, rpc.CodeInvalidToken)

	errVal, result = c.call("auth.token_revoke", f.rootTok, int64(0), "laptop")
	if errVal != nil {
		t.Fatalf("token_revoke: %#v", errVal)
	}
	if result.(map[string]any)["revoked"] != true {
		t.Fatalf("revoke reply: %#v", result)
	}
	// Revocation is a flag, not a delete: the row is what an audit trail
	// points at, and deleting it would leave every "authenticated with
	// token X" record naming something that no longer exists.
	_, result = c.call("auth.token_list", f.rootTok, int64(0))
	rows = result.([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["revoked"] != true {
		t.Fatalf("after revoke the listing is %#v; the row must survive as revoked", rows)
	}

	// Arity.
	for _, bad := range [][]any{
		{"auth.token_create", f.rootTok, "x"},
		{"auth.token_list", f.rootTok},
		{"auth.token_revoke", f.rootTok, int64(0)},
	} {
		if errVal, _ := c.call(bad[0].(string), bad[1:]...); errVal == nil {
			t.Errorf("%s accepted %d arguments", bad[0], len(bad)-1)
		}
	}
}

// The integer domain is validated before the multiply. time.Duration(days)*
// 24*time.Hour overflows, and overflow WRAPS: math.MinInt64+1 days came back
// as a positive duration of about a day, sailed past the core's range check,
// and created a token. The core check was correct and was simply handed a
// number that no longer meant what the caller sent.
func TestTokenCreate_DayCountCannotOverflowIntoAValidLifetime(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()
	// One front-door-eligible connection for the whole table: the lifetime
	// guard must be what refuses these, not the binding gates.
	fdConn := f.frontDoorConn(t)

	for _, days := range []int64{
		math.MinInt64 + 1, // wrapped to ~+24h before the fix
		math.MinInt64,
		math.MaxInt64,
		-1,
		366,
	} {
		errVal, _ := c.call("auth.token_create", f.rootTok, fmt.Sprintf("t%d", days), days, "", fdConn, int64(0))
		if errVal == nil {
			t.Errorf("days = %d created a token; an out-of-range lifetime must be refused "+
				"before any arithmetic can turn it into a plausible one", days)
			continue
		}
		m, _ := errVal.(map[string]any)
		if code, _ := m["code"].(int64); code != rpc.CodeInvalidToken {
			t.Errorf("days = %d gave code %d, want CodeInvalidToken", days, code)
		}
	}

	// Positive control: the range that IS valid still works, so the guard is
	// not simply refusing everything.
	if errVal, _ := c.call("auth.token_create", f.rootTok, "ok-365", int64(365), "", f.frontDoorConn(t), int64(0)); errVal != nil {
		t.Errorf("365 days was refused: %#v", errVal)
	}
	if errVal, _ := c.call("auth.token_create", f.rootTok, "ok-default", int64(0), "", f.frontDoorConn(t), int64(0)); errVal != nil {
		t.Errorf("0 days (the default) was refused: %#v", errVal)
	}
}

// MF2: revoking a name the user does not have is a normal refusal, not a
// server fault. RevokePAT wrapped dao.ErrNoRows, which wireErr had no public
// mapping for, so a mistyped token name came back as -32603 "internal error"
// — telling someone who made a typo that the SERVER broke.
func TestTokenRevoke_MissingNameIsARefusalNotAFault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	errVal, _ := c.call("auth.token_revoke", f.rootTok, int64(0), "no-such-token")
	if errVal == nil {
		t.Fatal("revoking a name that does not exist reported success")
	}
	m, _ := errVal.(map[string]any)
	code, _ := m["code"].(int64)
	if code == -32603 {
		t.Fatalf("a missing token name came back as an internal fault (%#v); a typo is the "+
			"caller's business and must not read as a server failure", m)
	}
	if code != rpc.CodeInvalidToken {
		t.Errorf("code = %d, want CodeInvalidToken", code)
	}

	// Positive control: revoking one that DOES exist still works, so the
	// mapping is not simply refusing everything.
	if errVal, _ := c.call("auth.token_create", f.rootTok, "real", int64(0), "", f.frontDoorConn(t), int64(0)); errVal != nil {
		t.Fatalf("token_create: %#v", errVal)
	}
	if errVal, _ := c.call("auth.token_revoke", f.rootTok, int64(0), "real"); errVal != nil {
		t.Fatalf("revoking an existing token failed: %#v", errVal)
	}
}

// auth.ip_admitted answers the two-layer admission question for an address
// the daemon cannot observe: the web gateway reaches it over loopback, so
// the peer the daemon sees is the gateway rather than the browser.
//
// The DECISION stays with the daemon. A gateway evaluating prefixes itself
// would be a second implementation of admission in a second process, and the
// day they disagree is a day someone is admitted somewhere the rules say
// they are not.
func TestIPAdmittedOverWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.dial(t)
	c.hello()

	// The fixture's config allowlist carries loopback, so the GLOBAL layer
	// admits it with no user rows at all — the case Amendment 1 exists for.
	errVal, result := c.call("auth.ip_admitted", f.rootTok, "127.0.0.1")
	if errVal != nil {
		t.Fatalf("ip_admitted: %#v", errVal)
	}
	m := result.(map[string]any)
	if m["admitted"] != true {
		t.Fatalf("a globally-listed address was not admitted: %#v", m)
	}
	if m["source"] != "global" {
		t.Errorf("source = %v, want \"global\" — the audit must say which layer admitted", m["source"])
	}

	// An address in neither layer is refused. Without this the assertion
	// above would pass against a verb that admits everything.
	errVal, result = c.call("auth.ip_admitted", f.rootTok, "203.0.113.7")
	if errVal != nil {
		t.Fatalf("ip_admitted: %#v", errVal)
	}
	m = result.(map[string]any)
	if m["admitted"] != false {
		t.Fatalf("an address in neither layer was admitted: %#v", m)
	}
	if m["source"] != "none" {
		t.Errorf("source = %v, want \"none\"", m["source"])
	}

	// It needs a token: admission is a question about a USER, and answering
	// it for an unauthenticated caller would leak whose rules admit what.
	if errVal, _ := c.call("auth.ip_admitted", "not-a-token", "127.0.0.1"); errVal == nil {
		t.Error("ip_admitted answered without a valid token")
	}
	for _, bad := range [][]any{{f.rootTok}, {f.rootTok, "1.2.3.4", "extra"}} {
		if errVal, _ := c.call("auth.ip_admitted", bad...); errVal == nil {
			t.Errorf("ip_admitted accepted %d argument(s)", len(bad))
		}
	}
}

// frontdoor.endpoint has to report the LIVE listener, and every field of it.
//
// The failure this cell exists for is the quiet one: a field added to
// FrontDoorInfo and not to the map the handler returns. The struct compiles,
// the caller decodes a zero, and the consumer that keyed on it — here, the TUI
// banner and the card's sslmode — reports the SAFE state for a listener that is
// in the dangerous one. Nothing else in the suite would notice.
func TestFrontDoorEndpoint_ReportsEveryField(t *testing.T) {
	info := rpc.FrontDoorInfo{
		Enabled:    true,
		Listening:  true,
		Addr:       "127.0.0.1:6432",
		HostNames:  []string{"autodb.example.com", "alt.example.com"},
		RootCAFile: "/etc/autodb/tls/ca.pem",
		Cleartext:  true,
	}
	f := newFixture(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo { return info }))
	c := f.session(t)

	errVal, res := c.call("frontdoor.endpoint", f.rootTok)
	if errVal != nil {
		t.Fatalf("frontdoor.endpoint: %v", errVal)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", res)
	}
	for k, want := range map[string]any{
		"enabled":      true,
		"listening":    true,
		"addr":         "127.0.0.1:6432",
		"root_ca_file": "/etc/autodb/tls/ca.pem",
		"cleartext":    true,
	} {
		if got, ok := m[k]; !ok {
			t.Errorf("the reply has no %q key at all", k)
		} else if got != want {
			t.Errorf("%q = %v, want %v", k, got, want)
		}
	}
	names, _ := m["host_names"].([]any)
	if len(names) != 2 || names[0] != "autodb.example.com" {
		t.Errorf("host_names = %v, want both certificate names in order", m["host_names"])
	}

	// And the FALSE case travels too. A handler hardcoding true would satisfy
	// every assertion above.
	info.Cleartext = false
	if _, res := c.call("frontdoor.endpoint", f.rootTok); res.(map[string]any)["cleartext"] != false {
		t.Errorf("cleartext = %v for a TLS listener, want false", res.(map[string]any)["cleartext"])
	}
}

// The verb is privileged: "where does this install expose a database surface"
// is not a question an unauthenticated caller gets to ask.
func TestFrontDoorEndpoint_RequiresAToken(t *testing.T) {
	f := newFixture(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo {
		return rpc.FrontDoorInfo{Enabled: true, Listening: true, Addr: "127.0.0.1:6432"}
	}))
	c := f.dial(t)
	c.hello()
	errVal, _ := c.call("frontdoor.endpoint", "not-a-real-token")
	if errVal == nil {
		t.Fatal("frontdoor.endpoint answered an unauthenticated caller")
	}
}
