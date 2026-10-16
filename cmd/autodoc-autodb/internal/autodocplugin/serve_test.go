package autodocplugin

import (
	"context"
	"net"
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

// Through AutoDoc's own SDK and link: the run command carries its note, the plugin answers with
// a table named by the cursor, and each plugin.table.fetch is answered with host.rows until the
// last says no more.
func TestTheRunAndItsPagesCrossTheRealLink(t *testing.T) {
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) { return page(1, 2, true, "cur-x"), nil }
	next := 3
	db.fetch = func(_ string, n int64) (map[string]any, error) {
		to := min(next+int(n)-1, 7)
		pg := page(next, to, to < 7, "")
		next = to + 1
		return pg, nil
	}
	p := newPlugin("v0.0.1", t.TempDir())
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostEnd, pluginEnd := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- plugin.ServeConn(ctx, pluginEnd, p) }()
	got := make(chan note, 256)
	link, err := plugin.NewLink(ctx, hostEnd, func(method string, params []any) { got <- note{method, params} })
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	send := func(method string, params []any) {
		t.Helper()
		if err := link.Notify(ctx, method, params); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	// waitFor is the next notification of method, skipping frames and the like
	waitFor := func(method string) note {
		t.Helper()
		// NOTE: a test's deadline: a plugin that never answers fails the cell rather than hang it.
		deadline := time.After(10 * time.Second)
		for {
			select {
			case n := <-got:
				if n.method == method {
					return n
				}
			case <-deadline:
				t.Fatalf("no %s within 10 s", method)
			}
		}
	}

	send(plugin.MethodOpen, plugin.OpenParams(plugin.Open{Protocol: 4, Width: 60, Height: 24}))
	waitFor(plugin.MethodReady)
	key := func(k string) { send(plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: k, Text: k})) }
	for _, r := range "admin" {
		key(string(r))
	}
	send(plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: "Tab"}))
	for _, r := range "right" {
		key(string(r))
	}
	send(plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: "Enter"})) // sign in
	send(plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: "Enter"})) // the one connection

	doc := docAt("SELECT n FROM t|")
	send(plugin.MethodCommand, plugin.FitCommand("run", doc))
	tab, _, err := plugin.ReadTable(waitFor(plugin.MethodTable).params)
	if err != nil {
		t.Fatal(err)
	}
	if tab.ID != "cur-x" || !tab.More || len(tab.Rows) != 2 {
		t.Fatalf("host.table %+v, want cursor cur-x's first 2 rows with more", tab)
	}
	rows := len(tab.Rows)
	for more := true; more; {
		send(plugin.MethodFetch, plugin.FetchParams(plugin.TableFetch{ID: tab.ID, N: 2}))
		id, page, m, _, err := plugin.ReadRows(waitFor(plugin.MethodRows).params)
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
	send(plugin.MethodClose, plugin.EmptyParams())
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after plugin.close")
	}
}
