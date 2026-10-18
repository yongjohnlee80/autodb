package autodocplugin

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// treeDB is a fake AutoDB whose scratch connection holds public (users, a view "Order Lines" with
// no columns, legacy from a server that quotes no columns) and audit (no tables).
func treeDB(t *testing.T) *fakeDB {
	t.Helper()
	db := newFakeDB(t)
	db.schemas = []string{"public", "audit"}
	db.tables = map[string][]map[string]any{
		"public": {
			{"schema": "public", "name": "users", "kind": "table", "quoted": `"public"."users"`},
			{"schema": "public", "name": "Order Lines", "kind": "view", "quoted": `"public"."Order Lines"`},
			{"schema": "public", "name": "legacy", "kind": "table", "quoted": `"public"."legacy"`},
		},
	}
	db.columns = map[string][]map[string]any{
		"public.users": {
			{"name": "id", "type": "integer", "pk": true, "quoted": `"id"`},
			{"name": "Full Name", "type": "text", "quoted": `"Full Name"`},
		},
		"public.legacy": {
			{"name": "id", "type": "integer", "pk": true},
			{"name": "note; DROP TABLE users", "type": "text"},
		},
	}
	return db
}

// scratch is the scratch connection's node: in the workspace AutoDoc.
var scratch = []string{"w1", "c7"}

func at(keys ...string) []string { return append(append([]string(nil), scratch...), keys...) }

// sentTree records the tree the plugin sends the panel: each node's children, by its keys.
type sentTree map[string][]panel.Node

func recordTree(p *Plugin) sentTree {
	got := sentTree{}
	p.treeSent = func(parent []string, nodes []panel.Node) { got[strings.Join(parent, "/")] = nodes }
	return got
}

func (s sentTree) labels(keys ...string) []string {
	var out []string
	for _, n := range s[strings.Join(keys, "/")] {
		out = append(out, n.Label)
	}
	return out
}

// The workspaces are the tree's top, each with its connections; a connection in no workspace is
// listed under one of its own, so it can still be chosen.
func TestTheWorkspacesAreTheTreesTop(t *testing.T) {
	db := treeDB(t)
	db.loose = []map[string]any{{"id": int64(9), "name": "stray", "engine": "postgres"}}
	p, h := testPlugin(t, db)
	got := recordTree(p)
	p.attach(h)
	signIn(p, "right")
	if top := got.labels(); !slices.Equal(top, []string{"AutoDoc", "(no workspace)"}) {
		t.Fatalf("the top: %q", top)
	}
	p.opened([]string{"w1"})
	p.opened([]string{"w0"})
	if a, b := got.labels("w1"), got.labels("w0"); !slices.Equal(a, []string{"scratch (postgres)"}) || !slices.Equal(b, []string{"stray (postgres)"}) {
		t.Fatalf("the connections: %q and %q", a, b)
	}
	p.useNode([]string{"w0", "c9"})
	if p.step != stepReady || p.conn == nil || p.conn.Name != "stray" {
		t.Fatalf("Enter on stray: step %d (%s)", p.step, said(p))
	}
}

// Each level is read from AutoDB the first time its node opens: a connection's schemas, a
// schema's tables and views, a table's columns; an empty level says so.
func TestTheTreeOpensLevelByLevel(t *testing.T) {
	db := treeDB(t)
	p, _ := connected(t, db)
	got := recordTree(p)
	if n := len(db.took("schema.schemas")); n != 0 {
		t.Fatalf("connecting read the schemas %d times, want none until the connection opens", n)
	}
	p.opened(scratch)
	if s := got.labels(scratch...); !slices.Equal(s, []string{"public", "audit"}) {
		t.Fatalf("the schemas: %q", s)
	}
	p.opened(at("s:public"))
	if s := got.labels(at("s:public")...); !slices.Equal(s, []string{"users (table)", "Order Lines (view)", "legacy (table)"}) {
		t.Fatalf("public's tables: %q", s)
	}
	if tk := db.took("schema.tables"); len(tk) != 1 || tk[0].params[2] != "public" {
		t.Fatalf("opening public read %+v", tk)
	}
	if n := len(db.took("schema.columns")); n != 0 {
		t.Fatalf("listing the tables read columns %d times", n)
	}
	p.opened(at("s:public", "t:users"))
	if s := got.labels(at("s:public", "t:users")...); !slices.Equal(s, []string{"id integer · pk", "Full Name text"}) {
		t.Fatalf("users' columns: %q", s)
	}
	p.opened(at("s:public", "t:Order Lines"))
	if s := got.labels(at("s:public", "t:Order Lines")...); !slices.Equal(s, []string{"(no columns)"}) {
		t.Fatalf("a view with no columns: %q", s)
	}
	p.opened(at("s:audit"))
	if s := got.labels(at("s:audit")...); !slices.Equal(s, []string{"(no tables)"}) {
		t.Fatalf("a schema with no tables: %q", s)
	}
}

// A schema AutoDB will not list says why on the problem line, and in its node.
func TestATreeLoadErrorIsTheProblem(t *testing.T) {
	db := treeDB(t)
	db.schemas = []string{"broken"}
	p, _ := connected(t, db)
	got := recordTree(p)
	p.opened(at("s:broken"))
	if p.step != stepReady || !strings.Contains(said(p), "reading broken: exec: permission denied for schema broken") {
		t.Fatalf("the load error is not the problem (step %d): %s", p.step, said(p))
	}
	if s := got.labels(at("s:broken")...); len(s) != 1 || !strings.Contains(s[0], "could not be read") {
		t.Fatalf("the node says %q", s)
	}
}

// Enter on a table opens a SELECT of it in a new buffer: buffer.new with <schema>.<table>.sql and
// the scaffold, every identifier the server's quoting. A table whose columns the server did not
// quote is SELECT * of it: a raw name is never put in the text.
func TestEnterOnATableOpensASelect(t *testing.T) {
	db := treeDB(t)
	p, h := connected(t, db)
	p.opened(at("s:public"))
	p.useNode(at("s:public", "t:users"))
	want := hostCall{"buffer.new", []string{"public.users.sql",
		"SELECT\n  \"id\",\n  \"Full Name\"\nFROM \"public\".\"users\"\nLIMIT 100;\n"}}
	if len(h.calls) != 1 || fmt.Sprint(h.calls[0]) != fmt.Sprint(want) {
		t.Fatalf("Enter on users sent %q, want %q", h.calls, want)
	}
	if !strings.Contains(said(p), "opened public.users.sql") {
		t.Fatalf("the panel does not say the query opened: %s", said(p))
	}
	p.useNode(at("s:public", "t:legacy"))
	want = hostCall{"buffer.new", []string{"public.legacy.sql", "SELECT *\nFROM \"public\".\"legacy\"\nLIMIT 100;\n"}}
	if len(h.calls) != 2 || fmt.Sprint(h.calls[1]) != fmt.Sprint(want) {
		t.Fatalf("Enter on legacy sent %q, want %q", h.calls[1:], want)
	}
	if strings.Contains(h.calls[1].args[1], "DROP") {
		t.Fatal("a raw column name went into the query")
	}
}

// AutoDoc's answer to buffer.new: an AutoDoc without it says to update; a refusal or a failure
// gives its reason.
func TestBufferNewAnswersOnTheProblemLine(t *testing.T) {
	for _, c := range []struct {
		answer error
		want   string
	}{
		{&plugin.CallError{Code: plugin.CallUnknown, Message: "no command buffer.new"}, "this AutoDoc can't open a buffer: update AutoDoc"},
		{&plugin.CallError{Code: plugin.CallFailed, Message: "the text is over 256 KiB"}, "AutoDoc could not open public.users.sql: the text is over 256 KiB"},
		{&plugin.CallError{Code: plugin.CallRefused, Message: "buffer.new is not in the manifest's calls"}, "AutoDoc refused to open public.users.sql: buffer.new is not in the manifest's calls"},
	} {
		p, h := connected(t, treeDB(t))
		h.answer = c.answer
		p.opened(at("s:public"))
		p.useNode(at("s:public", "t:users"))
		if !strings.Contains(said(p), c.want) {
			t.Errorf("answered %v, the panel does not say %q: %s", c.answer, c.want, said(p))
		}
	}
}

// A buffer name loses the /, \ and .. AutoDoc refuses in one.
func TestABufferNameIsOneAutoDocTakes(t *testing.T) {
	for _, c := range []struct{ schema, table, want string }{
		{"public", "users", "public.users.sql"},
		{"", "users", "users.sql"},
		{"a/b", "../etc/x", "ab.etcx.sql"},
		{`c:\`, "..", "c:.sql"},
		{"..", "..", "query.sql"},
	} {
		got := bufferName(autodb.Table{Schema: c.schema, Name: c.table})
		if got != c.want || strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("schema %q table %q is named %q, want %q", c.schema, c.table, got, c.want)
		}
	}
}

// A column list that would make the scaffold larger than buffer.new takes is SELECT * instead.
func TestAScaffoldPastTheCapIsSelectStar(t *testing.T) {
	db := treeDB(t)
	var cols []map[string]any
	long := strings.Repeat("c", 120)
	for i := range 2500 {
		name := fmt.Sprintf("%s_%04d", long, i)
		cols = append(cols, map[string]any{"name": name, "type": "text", "quoted": `"` + name + `"`})
	}
	db.tables["public"] = append(db.tables["public"], map[string]any{"schema": "public", "name": "wide", "kind": "table", "quoted": `"public"."wide"`})
	db.columns["public.wide"] = cols
	if n := 2500 * (len(long) + 5 + 2 + 4); n <= maxBufferText {
		t.Fatalf("the cell's columns come to %d bytes, not past the %d cap", n, maxBufferText)
	}
	p, h := connected(t, db)
	p.opened(at("s:public"))
	p.useNode(at("s:public", "t:wide"))
	if len(h.calls) != 1 || h.calls[0].args[0] != "public.wide.sql" || h.calls[0].args[1] != "SELECT *\nFROM \"public\".\"wide\"\nLIMIT 100;\n" {
		t.Fatalf("Enter on wide sent %d calls, the first %.80q", len(h.calls), h.calls)
	}
}
