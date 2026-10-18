package autodocplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// call is one request the fake AutoDB took.
type call struct {
	method string
	params []any
}

// fakeDB is an AutoDB in-process: golib's RPC server with the verbs the plugin calls, answering
// as AutoDB's wire does. A cell sets query and fetch; every request is recorded.
type fakeDB struct {
	mu    sync.Mutex
	calls []call
	addr  string
	// query answers exec.session_query; fetch answers exec.session_fetch.
	query func(sql string) (map[string]any, error)
	fetch func(cur string, n int64) (map[string]any, error)
	// engine is the connection's engine.
	engine string
	// storeID is the meta store the hello names, none when "".
	storeID string
	// pass is the passphrase auth.login takes, "right" when "".
	pass string
	// noUsers is a store with no user yet: auth.needs_bootstrap says so until auth.bootstrap
	noUsers bool
	// notes answer notes.list; notesDir is the hello's notes_dir
	notes    []string
	notesDir string
	// protocol is the hello's, autodb.Protocol when 0; a client's hello of another is refused.
	// nextProtocol is the protocol after a restart, the same when 0.
	protocol, nextProtocol int64
	// restart answers sys.restart_if_idle: "" stops (and restarts the fake), "busy", "denied",
	// "unknown" (a release before the verb); restarts counts the restarts
	restart  string
	restarts int
	// installs counts the plugin's installs (testPlugin's)
	installs int
	// the server: its verbs, its stop, and up, closed while it listens
	handlers map[string]golibrpc.Handler
	stop     func()
	up       chan struct{}
	// noWorkspaces answers workspace.list with none
	noWorkspaces bool
	// locked is a store restarted with no service keyslot: whoami says store_unlocked false;
	// revoked refuses every token auth.whoami is asked about
	locked, revoked bool
	// more are connections conn.list lists after the first, scratch (id 7), all in the workspace
	// AutoDoc (id 1); loose ones conn.list lists in no workspace.
	more, loose []map[string]any
	// schemas answer schema.schemas (public alone when nil); tables answer schema.tables by
	// schema, columns schema.columns by "schema.table", each row as AutoDB's wire carries it.
	schemas []string
	tables  map[string][]map[string]any
	columns map[string][]map[string]any
}

func newFakeDB(t *testing.T) *fakeDB {
	t.Helper()
	f := &fakeDB{engine: "postgres"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	f.handlers = map[string]golibrpc.Handler{}
	f.up = make(chan struct{})
	close(f.up)
	handle := func(method string, h func(p []any) (any, error)) {
		f.handlers[method] = (func(_ context.Context, req *golibrpc.Request) (any, error) {
			f.mu.Lock()
			f.calls = append(f.calls, call{method, req.Params})
			f.mu.Unlock()
			return h(req.Params)
		})
	}
	handle("sys.hello", func(p []any) (any, error) {
		f.mu.Lock()
		proto := f.protocol
		f.mu.Unlock()
		if proto == 0 {
			proto = autodb.Protocol
		}
		if info, _ := p[0].(map[string]any); info["protocol"] != nil && toInt(info["protocol"]) != proto {
			return nil, &golibrpc.Error{Code: autodb.CodeProtocolMismatch,
				Message: fmt.Sprintf("protocol mismatch: the server speaks protocol %d", proto)}
		}
		m := map[string]any{"protocol": proto, "server": "autodb", "version": "v-fake", "instance": "i1"}
		f.mu.Lock()
		if f.notesDir != "" {
			m["notes_dir"] = f.notesDir
		}
		if f.storeID != "" {
			m["store_id"] = f.storeID
		}
		f.mu.Unlock()
		return m, nil
	})
	handle("auth.needs_bootstrap", func([]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.noUsers, nil
	})
	handle("auth.bootstrap", func(p []any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.noUsers = false
		return map[string]any{"token": "tok-1", "user": map[string]any{"name": p[0]}}, nil
	})
	ok := func([]any) (any, error) { return nil, nil }
	for _, m := range []string{"workspace.rename", "workspace.delete", "workspace.attach", "conn.rename", "conn.test"} {
		handle(m, ok)
	}
	handle("notes.list", func([]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, n := range f.notes {
			out = append(out, n)
		}
		return out, nil
	})
	handle("notes.create", func(p []any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notes = append(f.notes, p[2].(string))
		return map[string]any{"version": "v0"}, nil
	})
	handle("notes.delete", ok)
	handle("sys.restart_if_idle", func([]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch f.restart {
		case "busy":
			return map[string]any{"stopping": false, "busy": map[string]any{"executing": int64(1), "in_transaction": int64(2)}}, nil
		case "denied":
			return nil, &golibrpc.Error{Code: autodb.CodeDenied, Message: "denied: admin only"}
		case "unknown":
			return nil, &golibrpc.Error{Code: golibrpc.CodeMethodNotFound, Message: "unknown method: sys.restart_if_idle"}
		}
		f.up = make(chan struct{})
		go f.bounce()
		return map[string]any{"stopping": true}, nil
	})
	handle("workspace.create", func([]any) (any, error) { return int64(2), nil })
	handle("conn.create", func([]any) (any, error) { return int64(8), nil })
	handle("conn.delete", func([]any) (any, error) { return map[string]any{"archived": false}, nil })
	handle("auth.login", func(p []any) (any, error) {
		want := "right"
		f.mu.Lock()
		if f.pass != "" {
			want = f.pass
		}
		f.mu.Unlock()
		if p[1] != want {
			return nil, &golibrpc.Error{Code: autodb.CodeAuth, Message: "auth: bad credentials"}
		}
		return map[string]any{"token": "tok-1", "user": map[string]any{"name": p[0]}}, nil
	})
	handle("auth.logout", func([]any) (any, error) { return nil, nil })
	handle("auth.whoami", func(p []any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p[0] != "tok-1" || f.revoked {
			return nil, &golibrpc.Error{Code: autodb.CodeAuth, Message: "auth: invalid or expired token"}
		}
		return map[string]any{"id": int64(1), "name": "admin", "role": "admin", "store_unlocked": !f.locked}, nil
	})
	handle("conn.list", func([]any) (any, error) {
		list := []any{map[string]any{"id": int64(7), "name": "scratch", "engine": f.engineName()}}
		f.mu.Lock()
		for _, c := range f.more {
			list = append(list, c)
		}
		for _, c := range f.loose {
			list = append(list, c)
		}
		f.mu.Unlock()
		return list, nil
	})
	handle("workspace.list", func([]any) (any, error) {
		f.mu.Lock()
		empty := f.noWorkspaces
		f.mu.Unlock()
		if empty {
			return []any{}, nil
		}
		conns := []any{map[string]any{"id": int64(7), "name": "scratch", "engine": f.engineName()}}
		f.mu.Lock()
		for _, c := range f.more {
			conns = append(conns, c)
		}
		f.mu.Unlock()
		return []any{map[string]any{"id": int64(1), "name": "AutoDoc", "connections": conns}}, nil
	})
	handle("schema.schemas", func([]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		names := f.schemas
		if names == nil {
			names = []string{"public"}
		}
		out := []any{}
		for _, n := range names {
			out = append(out, n)
		}
		return out, nil
	})
	handle("schema.tables", func(p []any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p[2] == "broken" {
			return nil, &golibrpc.Error{Code: -32032, Message: "exec: permission denied for schema broken"}
		}
		out := []any{}
		for _, t := range f.tables[p[2].(string)] {
			out = append(out, t)
		}
		return out, nil
	})
	handle("schema.columns", func(p []any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for _, c := range f.columns[p[2].(string)+"."+p[3].(string)] {
			out = append(out, c)
		}
		return out, nil
	})
	handle("exec.session_open", func([]any) (any, error) { return map[string]any{"session_id": "sess-1"}, nil })
	handle("exec.session_close", func([]any) (any, error) { return map[string]any{"closed": true}, nil })
	handle("exec.session_query", func(p []any) (any, error) {
		if f.query == nil {
			return nil, &golibrpc.Error{Code: -32603, Message: "no query set"}
		}
		return f.query(p[2].(string))
	})
	handle("exec.session_fetch", func(p []any) (any, error) {
		if f.fetch == nil {
			return nil, &golibrpc.Error{Code: autodb.CodeTxState, Message: "exec: no such cursor on this session"}
		}
		return f.fetch(p[2].(string), p[3].(int64))
	})
	handle("exec.session_cursor_close", func([]any) (any, error) { return map[string]any{"closed": true}, nil })
	handle("exec.run_script", func(p []any) (any, error) {
		res, err := f.query(p[2].(string))
		if err != nil {
			return nil, err
		}
		return map[string]any{"statements": int64(2), "result": res}, nil
	})
	f.serve(ln)
	t.Cleanup(func() {
		<-f.listening()
		f.mu.Lock()
		stop := f.stop
		f.mu.Unlock()
		stop()
	})
	return f
}

// serve runs a server of the fake's verbs on ln.
func (f *fakeDB) serve(ln net.Listener) {
	srv := golibrpc.New(msgpackrpc.New(nil), golibrpc.WithListener(ln))
	for m, h := range f.handlers {
		srv.Handle(m, h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()
	f.mu.Lock()
	f.stop = func() { cancel(); <-done }
	f.mu.Unlock()
}

// bounce is the server restarting (sys.restart_if_idle stopping): its connections end, it listens
// again at its address, at nextProtocol when one is set, and f.up is closed.
func (f *fakeDB) bounce() {
	f.mu.Lock()
	stop, up := f.stop, f.up
	f.restarts++
	if f.nextProtocol != 0 {
		f.protocol = f.nextProtocol
	}
	f.mu.Unlock()
	stop()
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		panic("the fake AutoDB could not listen again: " + err.Error())
	}
	f.serve(ln)
	close(up)
}

// listening is closed once the server listens: at once, or after a restart under way.
func (f *fakeDB) listening() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up
}

// toInt reads a wire integer.
func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case uint64:
		return int64(n)
	case int8:
		return int64(n)
	case uint8:
		return int64(n)
	case int16:
		return int64(n)
	case uint16:
		return int64(n)
	case int32:
		return int64(n)
	case uint32:
		return int64(n)
	}
	return -1
}

func (f *fakeDB) engineName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.engine
}

// took is the requests of method, in order.
func (f *fakeDB) took(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// queries are the SQL texts exec.session_query was sent.
func (f *fakeDB) queries() []string {
	var out []string
	for _, c := range f.took("exec.session_query") {
		out = append(out, c.params[2].(string))
	}
	return out
}

// page is a result as AutoDB's wire carries it: one column n, rows from..to, more, and a cursor
// when cur is not "".
func page(from, to int, more bool, cur string) map[string]any {
	rows := []any{}
	for i := from; i <= to; i++ {
		rows = append(rows, []any{int64(i)})
	}
	m := map[string]any{"verb": "select", "class": "read", "columns": []any{"n"}, "rows": rows,
		"more": more, "affected": int64(0), "duration_ms": int64(1)}
	if cur != "" {
		m["cursor"] = cur
	}
	return m
}

// fakeHost records what the plugin sends AutoDoc.
type fakeHost struct {
	mu     sync.Mutex
	frame  *plugin.Frame
	tables []plugin.TableOpen
	rows   []sentRows
	ends   []sentEnd
	frames int      // host.frame sent
	closes int      // host.close sent
	titles []string // host.title sent, in order
	calls  []hostCall
	// answer is what a host.call is answered: nil runs it
	answer error
	// slot is the session slot as last set (host.session); reason the close's (CloseReason)
	slot   map[string]string
	reason string
}

func (h *fakeHost) SetSession(values map[string]string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slot = maps.Clone(values)
	return nil
}

func (h *fakeHost) CloseReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

// hostCall is one host.call the plugin sent: the command's id and its args.
type hostCall struct {
	id   string
	args []string
}

func (h *fakeHost) Call(id string, args ...string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, hostCall{id, append([]string(nil), args...)})
	if h.answer != nil {
		return false, h.answer
	}
	return true, nil
}

type sentRows struct {
	id   string
	rows [][]string
	more bool
}

type sentEnd struct{ id, why string }

func (h *fakeHost) Frame(f *plugin.Frame) error {
	h.mu.Lock()
	h.frame = f
	h.frames++
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) Table(t plugin.TableOpen) error {
	h.mu.Lock()
	h.tables = append(h.tables, t)
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) Rows(id string, rows [][]string, more bool) error {
	h.mu.Lock()
	h.rows = append(h.rows, sentRows{id, rows, more})
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) TableEnd(id string, err error) error {
	why := ""
	if err != nil {
		why = err.Error()
	}
	h.mu.Lock()
	h.ends = append(h.ends, sentEnd{id, why})
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) Close() error {
	h.mu.Lock()
	h.closes++
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) Title(t string) error {
	h.mu.Lock()
	h.titles = append(h.titles, t)
	h.mu.Unlock()
	return nil
}

// title is the card's title as last sent, "" when none was.
func (h *fakeHost) title() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.titles) == 0 {
		return ""
	}
	return h.titles[len(h.titles)-1]
}

// closed is how many times the plugin asked to close the card.
func (h *fakeHost) closed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closes
}

// lines are the last frame's rows as text, the margins trimmed.
func (h *fakeHost) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.frame == nil {
		return nil
	}
	var out []string
	for _, row := range h.frame.Rows() {
		var b strings.Builder
		for _, r := range row {
			b.WriteString(r.Text)
		}
		out = append(out, strings.TrimSpace(b.String()))
	}
	return out
}

// styleOf is the style of the first cell of the last frame's row y that holds text, its text
// trimmed of spaces at the start.
func (h *fakeHost) styleOf(y int, text string) (plugin.Style, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.frame == nil || y < 0 || y >= len(h.frame.Rows()) {
		return plugin.Style{}, false
	}
	for _, r := range h.frame.Rows()[y] {
		if strings.Contains(r.Text, text) {
			return r.Style, true
		}
	}
	return plugin.Style{}, false
}

// card is the last frame's text, its rows joined.
func (h *fakeHost) card() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.frame == nil {
		return ""
	}
	var b strings.Builder
	for _, row := range h.frame.Rows() {
		for _, r := range row {
			b.WriteString(r.Text)
		}
		b.WriteByte('\n')
	}
	// a wrapped sentence reads whole
	return strings.Join(strings.Fields(b.String()), " ")
}

// testPlugin is a plugin over db, run inline (each step at once on the test's goroutine), its
// panel not opened: the cells read its state, and serve_test.go drives the panel over the link.
func testPlugin(t *testing.T, db *fakeDB) (*Plugin, *fakeHost) {
	t.Helper()
	p := newPlugin("v0.0.1", t.TempDir())
	p.inline = true
	t.Cleanup(p.Close)
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb", Version: "v-fake", Via: "test"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		<-db.listening() // a restart under way: the server listens again
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	p.install = func(context.Context) error { db.mu.Lock(); db.installs++; db.mu.Unlock(); return nil }
	return p, &fakeHost{}
}

// connected is a plugin signed in to db and connected to its first connection, sending to h.
func connected(t *testing.T, db *fakeDB) (*Plugin, *fakeHost) {
	t.Helper()
	p, h := testPlugin(t, db)
	p.attach(h)
	if p.step != stepLogin {
		t.Fatalf("after open the plugin is at step %d, want the sign-in (%s)", p.step, said(p))
	}
	signIn(p, "right")
	if p.step != stepPick {
		t.Fatalf("after signing in the plugin is at step %d, want the connections (%s)", p.step, said(p))
	}
	p.useNode([]string{"w1", "c7"})
	if p.step != stepReady {
		t.Fatalf("after choosing the plugin is at step %d, want ready (%s)", p.step, said(p))
	}
	return p, h
}

// signIn is the Sign in dialog accepted with the name "admin" and pass.
func signIn(p *Plugin, pass string) { p.events().SignIn("admin", pass) }

// said is what the panel says: the status, the problem and the step's own text.
func said(p *Plugin) string {
	return strings.Join([]string{p.where(), p.status, p.problem, p.stepText()}, " · ")
}

// docAt is a note of text with the cursor at the byte where mark (removed) was.
func docAt(text string) plugin.Document {
	at := strings.Index(text, "|")
	if at < 0 {
		panic(fmt.Sprintf("docAt: no | in %q", text))
	}
	return plugin.Document{Path: "q.sql", Text: text[:at] + text[at+1:], CursorByte: at, Version: 1}
}

// freshPass is a passphrase made for the run, never written down.
func freshPass(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "Pp" + hex.EncodeToString(b[:])
}
