package autodocplugin

import (
	"context"
	"fmt"
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
	// more are connections conn.list lists after the first, scratch (id 7).
	more []map[string]any
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
	srv := golibrpc.New(msgpackrpc.New(nil), golibrpc.WithListener(ln))
	handle := func(method string, h func(p []any) (any, error)) {
		srv.Handle(method, func(_ context.Context, req *golibrpc.Request) (any, error) {
			f.mu.Lock()
			f.calls = append(f.calls, call{method, req.Params})
			f.mu.Unlock()
			return h(req.Params)
		})
	}
	handle("sys.hello", func([]any) (any, error) {
		return map[string]any{"protocol": autodb.Protocol, "server": "autodb", "version": "v-fake", "instance": "i1"}, nil
	})
	handle("auth.needs_bootstrap", func([]any) (any, error) { return false, nil })
	handle("auth.login", func(p []any) (any, error) {
		if p[1] != "right" {
			return nil, &golibrpc.Error{Code: autodb.CodeAuth, Message: "auth: bad credentials"}
		}
		return map[string]any{"token": "tok-1", "user": map[string]any{"name": p[0]}}, nil
	})
	handle("auth.logout", func([]any) (any, error) { return nil, nil })
	handle("conn.list", func([]any) (any, error) {
		list := []any{map[string]any{"id": int64(7), "name": "scratch", "engine": f.engineName()}}
		f.mu.Lock()
		for _, c := range f.more {
			list = append(list, c)
		}
		f.mu.Unlock()
		return list, nil
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
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

// connected is a plugin signed in to db and connected to its one connection, sending to h.
func connected(t *testing.T, db *fakeDB) (*Plugin, *fakeHost) {
	t.Helper()
	p := newPlugin("v0.0.1", t.TempDir())
	t.Cleanup(p.Close)
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb", Version: "v-fake", Via: "test"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	h := &fakeHost{}
	p.attach(h, 60, 24)
	if p.step != stepLogin {
		t.Fatalf("after open the card is at step %d, want the sign-in (%s)", p.step, h.card())
	}
	signIn(p, "right")
	if p.step != stepPick {
		t.Fatalf("after signing in the card is at step %d, want the connections (%s)", p.step, h.card())
	}
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepReady {
		t.Fatalf("after choosing the card is at step %d, want ready (%s)", p.step, h.card())
	}
	return p, h
}

// signIn types the name "admin" and pass into the card and presses Enter in the passphrase.
func signIn(p *Plugin, pass string) {
	p.field = focusName
	p.Key(plugin.Key{Key: "u", Ctrl: true}) // a name left from an earlier sign-in goes
	for _, r := range "admin" {
		p.Key(plugin.Key{Key: string(r), Text: string(r)})
	}
	p.Key(plugin.Key{Key: "Tab"})
	for _, r := range pass {
		p.Key(plugin.Key{Key: string(r), Text: string(r)})
	}
	p.Key(plugin.Key{Key: "Enter"})
}

// docAt is a note of text with the cursor at the byte where mark (removed) was.
func docAt(text string) plugin.Document {
	at := strings.Index(text, "|")
	if at < 0 {
		panic(fmt.Sprintf("docAt: no | in %q", text))
	}
	return plugin.Document{Path: "q.sql", Text: text[:at] + text[at+1:], CursorByte: at, Version: 1}
}
