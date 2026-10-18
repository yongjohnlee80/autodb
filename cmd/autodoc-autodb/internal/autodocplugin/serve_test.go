package autodocplugin

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// note is one notification the plugin sent the host.
type note struct {
	method string
	params []any
}

// linked is a plugin over db served through AutoDoc's own SDK and link, as AutoDoc runs it: the
// panel's program runs, its frames cross the link, and the steps run on the plugin's goroutine.
type linked struct {
	t     *testing.T
	ctx   context.Context
	link  *plugin.Link
	got   chan note
	mu    sync.Mutex
	frame string // the latest frame's text, its rows one a line
}

func serveOn(t *testing.T, db *fakeDB) *linked {
	t.Helper()
	p := newPlugin("v0.0.1", t.TempDir())
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	hostEnd, pluginEnd := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- plugin.ServeConn(ctx, pluginEnd, p) }()
	l := &linked{t: t, ctx: ctx, got: make(chan note, 1024)}
	link, err := plugin.NewLink(ctx, hostEnd, func(method string, params []any) {
		if method == plugin.MethodFrame {
			if rows, _, err := plugin.ReadFrame(params); err == nil {
				var b strings.Builder
				for _, r := range rows {
					for _, run := range r {
						b.WriteString(run.Text)
					}
					b.WriteByte('\n')
				}
				l.mu.Lock()
				l.frame = b.String()
				l.mu.Unlock()
			}
			return
		}
		l.got <- note{method, params}
	})
	if err != nil {
		t.Fatal(err)
	}
	l.link = link
	t.Cleanup(func() {
		_ = link.Notify(ctx, plugin.MethodClose, plugin.EmptyParams())
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after plugin.close")
		}
		link.Close()
		cancel()
	})
	l.send(plugin.MethodOpen, plugin.OpenParams(plugin.Open{Protocol: 6, Width: 60, Height: 24}))
	l.waitFor(plugin.MethodReady)
	l.send(plugin.MethodFocus, plugin.FocusParams(true))
	return l
}

func (l *linked) send(method string, params []any) {
	l.t.Helper()
	if err := l.link.Notify(l.ctx, method, params); err != nil {
		l.t.Fatalf("send %s: %v", method, err)
	}
}

func (l *linked) key(k string) {
	text := ""
	if len([]rune(k)) == 1 {
		text = k
	}
	l.send(plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: k, Text: text}))
}

func (l *linked) typed(s string) {
	for _, r := range s {
		l.key(string(r))
	}
}

// waitFor is the next notification of method, skipping the others.
func (l *linked) waitFor(method string) note {
	l.t.Helper()
	// NOTE: a test's deadline: a plugin that never answers fails the cell rather than hang it.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case n := <-l.got:
			if n.method == method {
				return n
			}
		case <-deadline:
			l.t.Fatalf("no %s within 10 s; the panel:\n%s", method, l.text())
		}
	}
}

func (l *linked) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frame
}

// shows waits for the panel to draw s.
func (l *linked) shows(s string) {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(l.text(), s) {
		if time.Now().After(deadline) {
			l.t.Fatalf("the panel never showed %q:\n%s", s, l.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Through AutoDoc's own SDK and link, the panel as a user drives it: the Sign in dialog opens by
// itself, takes the name and the passphrase; the tree shows the workspace, which opens on its
// connection; Enter connects; the run command carries its note, the plugin answers with a table
// named by the cursor, and each plugin.table.fetch is answered with host.rows until the last says
// no more.
func TestThePanelSignsInConnectsAndRunsOverTheRealLink(t *testing.T) {
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) { return page(1, 2, true, "cur-x"), nil }
	next := 3
	db.fetch = func(_ string, n int64) (map[string]any, error) {
		to := min(next+int(n)-1, 7)
		pg := page(next, to, to < 7, "")
		next = to + 1
		return pg, nil
	}
	l := serveOn(t, db)
	l.shows("Sign in to AutoDB")
	l.typed("admin")
	l.key("Tab")
	l.typed("right")
	l.key("Enter")
	l.shows("AutoDoc")
	l.key("l") // the workspace opens on its connections
	l.shows("scratch (postgres)")
	l.key("j")
	l.key("Enter")
	l.shows("Connection: scratch (postgres)")
	for title := ""; !strings.Contains(title, "scratch"); { // "AutoDB" first, then the connection's
		title, _ = plugin.ReadTitle(l.waitFor(plugin.MethodTitle).params)
	}

	l.send(plugin.MethodCommand, plugin.FitCommand("run", docAt("SELECT n FROM t|")))
	tab, _, err := plugin.ReadTable(l.waitFor(plugin.MethodTable).params)
	if err != nil {
		t.Fatal(err)
	}
	if tab.ID != "cur-x" || !tab.More || len(tab.Rows) != 2 {
		t.Fatalf("host.table %+v, want cursor cur-x's first 2 rows with more", tab)
	}
	rows := len(tab.Rows)
	for more := true; more; {
		l.send(plugin.MethodFetch, plugin.FetchParams(plugin.TableFetch{ID: tab.ID, N: 2}))
		id, page, m, _, err := plugin.ReadRows(l.waitFor(plugin.MethodRows).params)
		if err != nil || id != tab.ID {
			t.Fatalf("host.rows for %q: %v", id, err)
		}
		rows += len(page)
		more = m
	}
	if rows != 7 {
		t.Fatalf("%d rows crossed the link, want 7", rows)
	}
	if q := db.queries(); len(q) != 1 || q[0] != "SELECT n FROM t" {
		t.Fatalf("the command ran %q, want the statement of the note it carried", q)
	}

	// :autodb query select 1 — the command's arguments are the SQL (protocol 7)
	l.send(plugin.MethodCommand, plugin.CommandArgsParams("query", []string{"select", "1"}))
	if _, _, err := plugin.ReadTable(l.waitFor(plugin.MethodTable).params); err != nil {
		t.Fatal(err)
	}
	if q := db.queries(); len(q) != 2 || q[1] != "select 1" {
		t.Fatalf(":autodb query select 1 ran %q", q)
	}
}

// A refused sign-in opens the dialog again, saying why; the ☰ menu's rows follow the state.
func TestARefusedSignInReopensTheDialogOverTheRealLink(t *testing.T) {
	l := serveOn(t, newFakeDB(t))
	l.shows("Sign in to AutoDB")
	l.typed("admin")
	l.key("Tab")
	l.typed("wrong")
	l.key("Enter")
	l.shows("sign-in refused")
	l.shows("Sign in to AutoDB") // open again
	l.typed("admin")
	l.key("Tab")
	l.typed("right")
	l.key("Enter")
	l.shows("AutoDoc")
}
