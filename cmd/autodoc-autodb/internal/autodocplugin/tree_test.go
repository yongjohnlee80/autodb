package autodocplugin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
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

// press sends keys to the card, one a name.
func press(p *Plugin, keys ...string) {
	for _, k := range keys {
		p.Key(plugin.Key{Key: k, Text: k})
	}
}

// chosenRow is the text of the tree's selected row, the "> " row in the selection's style, its
// spaces run together; "" when none is.
func chosenRow(h *fakeHost, p *Plugin) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, row := range h.frame.Rows() {
		for _, r := range row {
			if r.Style == p.chosenStyle() && strings.HasPrefix(r.Text, "> ") {
				return strings.Join(strings.Fields(r.Text), " ")
			}
		}
	}
	return ""
}

// A press chooses the row actually painted at that y, and a wheel advances the tree selection.
// This exercises the card's scroll mapping, including a fixed heading above the moving rows.
func TestPointerSelectsAndScrollsTheReadyCard(t *testing.T) {
	p, h := connected(t, treeDB(t))
	p.Resize(48, 14)
	audit := rowOf(h.lines(), 0, has("audit"))
	if audit < 0 {
		t.Fatalf("audit is not visible: %q", h.lines())
	}
	p.Pointer(plugin.Pointer{Kind: "press", Button: "left", X: 4, Y: audit, Count: 1})
	if p.tree.sel != 1 || !strings.Contains(chosenRow(h, p), "audit") {
		t.Fatalf("press on the second schema selected %d: %q", p.tree.sel, chosenRow(h, p))
	}
	p.Pointer(plugin.Pointer{Kind: "wheel", Button: "up", X: 4, Y: 4, Count: 1})
	if p.tree.sel != 0 || !strings.Contains(chosenRow(h, p), "public") {
		t.Fatalf("wheel up selected %d: %q", p.tree.sel, chosenRow(h, p))
	}
	press(p, "Enter") // expand public, creating enough rows to scroll the short card
	p.Resize(48, 10)
	p.Pointer(plugin.Pointer{Kind: "wheel", Button: "down", X: 4, Y: 4, Count: 2})
	last := len(p.tree.items()) - 1
	if p.tree.sel != last || p.scroll == 0 || chosenRow(h, p) == "" {
		t.Fatalf("wheel did not reveal the selected row: selected %d, scroll %d, row %q", p.tree.sel, p.scroll, chosenRow(h, p))
	}
	p.Pointer(plugin.Pointer{Kind: "press", Button: "left", X: 4, Y: p.h - 1, Count: 1})
	if p.tree.sel != last {
		t.Fatal("press on the footer moved the tree selection")
	}
}

// The ready card lists no help rows: SPC p shows the commands itself. Its keys row names the
// tree's keys, another connection and close.
func TestTheReadyCardHasNoHelpRows(t *testing.T) {
	p, h := connected(t, treeDB(t))
	p.Resize(48, 24)
	card := h.card()
	if strings.Contains(card, "SPC p") {
		t.Fatalf("the ready card still lists the SPC p rows:\n%s", strings.Join(h.lines(), "\n"))
	}
	for _, want := range []string{"j k: move", "l h: open, close", "Enter: query", "c: another connection", "q: close"} {
		if !strings.Contains(card, want) {
			t.Errorf("the keys row does not name %q:\n%s", want, strings.Join(h.lines(), "\n"))
		}
	}
}

// The tree reads each level as it is opened: the schemas on connect, a schema's tables when it is
// opened, a table's columns when it is opened. l and → open, h and ← close or step out, j k and
// the arrows move the bar.
func TestTheTreeOpensLevelByLevel(t *testing.T) {
	db := treeDB(t)
	p, h := connected(t, db)
	p.Resize(48, 24)
	if got := chosenRow(h, p); got != "> ▸ public" {
		t.Fatalf("on connect the bar is on %q, want public:\n%s", got, strings.Join(h.lines(), "\n"))
	}
	if rowOf(h.lines(), 0, is("▸ audit")) < 0 {
		t.Fatalf("the schemas are not listed:\n%s", strings.Join(h.lines(), "\n"))
	}
	if n := len(db.took("schema.tables")); n != 0 {
		t.Fatalf("connecting read %d schemas' tables, want none until one is opened", n)
	}

	press(p, "l")
	lines := h.lines()
	if chosenRow(h, p) != "> ▾ public" || rowOf(lines, 0, is("▸ users (table)")) < 0 || rowOf(lines, 0, is("▸ Order Lines (view)")) < 0 {
		t.Fatalf("public opened:\n%s", strings.Join(lines, "\n"))
	}
	if tk := db.took("schema.tables"); len(tk) != 1 || tk[0].params[2] != "public" {
		t.Fatalf("opening public read %+v", tk)
	}
	press(p, "Down")
	if got := chosenRow(h, p); got != "> ▸ users (table)" {
		t.Fatalf("Down moved the bar to %q", got)
	}
	if n := len(db.took("schema.columns")); n != 0 {
		t.Fatalf("reaching users read its columns %d times before it was opened", n)
	}
	press(p, "Right")
	lines = h.lines()
	if rowOf(lines, 0, is("id integer · pk")) < 0 || rowOf(lines, 0, is("Full Name text")) < 0 {
		t.Fatalf("users opened:\n%s", strings.Join(lines, "\n"))
	}
	press(p, "j")
	if got := chosenRow(h, p); got != "> id integer · pk" {
		t.Fatalf("j moved the bar to %q", got)
	}
	press(p, "h")
	if got := chosenRow(h, p); got != "> ▾ users (table)" {
		t.Fatalf("h on a column stepped to %q, want its table", got)
	}
	press(p, "Left")
	if got := chosenRow(h, p); got != "> ▸ users (table)" || rowOf(h.lines(), 0, is("id integer · pk")) >= 0 {
		t.Fatalf("← on an open table left the bar on %q:\n%s", got, strings.Join(h.lines(), "\n"))
	}
	press(p, "h")
	if got := chosenRow(h, p); got != "> ▾ public" {
		t.Fatalf("h on a closed table stepped to %q, want its schema", got)
	}
	press(p, "j", "j", "l")
	if rowOf(h.lines(), 0, is("(no columns)")) < 0 {
		t.Fatalf("a view with no columns:\n%s", strings.Join(h.lines(), "\n"))
	}
	press(p, "k", "k", "Enter")
	if got := chosenRow(h, p); got != "> ▸ public" {
		t.Fatalf("Enter on an open schema left %q, want it closed", got)
	}
	press(p, "j", "Enter")
	if rowOf(h.lines(), 0, is("(no tables)")) < 0 {
		t.Fatalf("Enter on a schema with no tables:\n%s", strings.Join(h.lines(), "\n"))
	}
	if n := len(db.took("schema.columns")); n != 2 {
		t.Fatalf("schema.columns was read %d times, want once for each table opened", n)
	}
}

// A schema AutoDB will not list says why on the problem line; the tree stays.
func TestATreeLoadErrorIsTheProblem(t *testing.T) {
	db := treeDB(t)
	db.schemas = []string{"broken"}
	p, h := connected(t, db)
	press(p, "l")
	if p.step != stepReady || !strings.Contains(h.card(), "listing broken's tables: exec: permission denied for schema broken") {
		t.Fatalf("the load error is not the problem (step %d):\n%s", p.step, strings.Join(h.lines(), "\n"))
	}
	if got := chosenRow(h, p); got != "> ▸ broken" {
		t.Fatalf("after the error the bar is on %q", got)
	}
}

// Enter on a table opens a SELECT of it in a new buffer: buffer.new with <schema>.<table>.sql and
// the scaffold, every identifier the server's quoting. A table whose columns the server did not
// quote is SELECT * of it: a raw name is never put in the text.
func TestEnterOnATableOpensASelect(t *testing.T) {
	db := treeDB(t)
	p, h := connected(t, db)
	p.Resize(48, 24)
	press(p, "l", "j", "Enter")
	want := hostCall{"buffer.new", []string{"public.users.sql",
		"SELECT\n  \"id\",\n  \"Full Name\"\nFROM \"public\".\"users\"\nLIMIT 100;\n"}}
	if len(h.calls) != 1 || fmt.Sprint(h.calls[0]) != fmt.Sprint(want) {
		t.Fatalf("Enter on users sent %q, want %q", h.calls, want)
	}
	if !strings.Contains(h.card(), "opened public.users.sql") {
		t.Fatalf("the card does not say the query opened:\n%s", strings.Join(h.lines(), "\n"))
	}
	press(p, "j", "j", "Enter")
	want = hostCall{"buffer.new", []string{"public.legacy.sql", "SELECT *\nFROM \"public\".\"legacy\"\nLIMIT 100;\n"}}
	if len(h.calls) != 2 || fmt.Sprint(h.calls[1]) != fmt.Sprint(want) {
		t.Fatalf("Enter on legacy sent %q, want %q:\n%s", h.calls[1:], want, strings.Join(h.lines(), "\n"))
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
		p.Resize(60, 24)
		h.answer = c.answer
		press(p, "l", "j", "Enter")
		if !strings.Contains(h.card(), c.want) {
			t.Errorf("answered %v, the card does not say %q:\n%s", c.answer, c.want, strings.Join(h.lines(), "\n"))
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
		got := bufferName(autodbTable(c.schema, c.table))
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
	press(p, "l", "j", "j", "j", "j", "Enter")
	if len(h.calls) != 1 || h.calls[0].args[0] != "public.wide.sql" || h.calls[0].args[1] != "SELECT *\nFROM \"public\".\"wide\"\nLIMIT 100;\n" {
		t.Fatalf("Enter on wide sent %d calls, the first %.80q", len(h.calls), h.calls)
	}
}

// A schema of thousands of tables scrolls: the bar stays in view, the view moves only as far as
// it must, the connection and the heading stay, and only the rows in view are made.
func TestTheTreeScrollsWithTheSelection(t *testing.T) {
	db := treeDB(t)
	var many []map[string]any
	for i := range 5000 {
		name := fmt.Sprintf("t%04d", i)
		many = append(many, map[string]any{"schema": "public", "name": name, "kind": "table", "quoted": `"public"."` + name + `"`})
	}
	db.tables["public"] = many
	p, h := connected(t, db)
	p.Resize(32, 12)
	press(p, "l")
	for range 100 {
		press(p, "j")
	}
	lines := h.lines()
	if got := chosenRow(h, p); got != "> ▸ t0099 (table)" {
		t.Fatalf("after 100 j the bar is on %q:\n%s", got, strings.Join(lines, "\n"))
	}
	if lines[0] != "Connection: scratch (postgres)" || lines[1] != "Ready" {
		t.Fatalf("scrolled, the card lost its fixed rows:\n%s", strings.Join(lines, "\n"))
	}
	press(p, "k")
	lines = h.lines()
	if chosenRow(h, p) != "> ▸ t0098 (table)" || rowOf(lines, 0, is("▸ t0099 (table)")) < 0 {
		t.Fatalf("k jumped the view instead of moving the bar within it:\n%s", strings.Join(lines, "\n"))
	}
	p.Key(plugin.Key{Key: "d", Ctrl: true})
	if got := chosenRow(h, p); got != fmt.Sprintf("> ▸ t%04d (table)", 98+max(12/2, 1)) {
		t.Fatalf("Ctrl+d moved the bar to %q", got)
	}
	press(p, "PageUp")
	if got := chosenRow(h, p); got != "> ▸ t0098 (table)" {
		t.Fatalf("PageUp moved the bar to %q", got)
	}

	made := 0
	l := p.lines(30)
	inner := l.list.row
	l.list.row = func(i int) row { made++; return inner(i) }
	l.from = p.scroll
	l.fit(12)
	if l.list.n < 5000 || made > 12 {
		t.Fatalf("a tree of %d rows made %d of them for a card 12 high", l.list.n, made)
	}
}

func autodbTable(schema, name string) autodb.Table { return autodb.Table{Schema: schema, Name: name} }
