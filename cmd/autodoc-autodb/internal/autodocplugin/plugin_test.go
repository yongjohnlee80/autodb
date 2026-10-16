package autodocplugin

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// whole answers every query with a whole result of one row.
func whole(string) (map[string]any, error) { return page(1, 1, false, ""), nil }

// run is the "run" command on doc, as AutoDoc sends it: the note, then the command.
func run(p *Plugin, doc plugin.Document) {
	p.Document(doc)
	p.Command("run")
}

// "run" sends the statement holding the cursor, as the engine's own splitter draws it: on a
// statement, on its semicolon, and in the space after it.
func TestRunSendsTheStatementAtTheCursor(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, _ := connected(t, db)
	for _, c := range []struct{ doc, want string }{
		{"SELECT 1;\nSELECT |2;\n", "SELECT 2"},
		{"SELECT |1;\nSELECT 2;\n", "SELECT 1"},
		{"SELECT 1|;\nSELECT 2;\n", "SELECT 1"},
		{"SELECT 1;  |\nSELECT 2;\n", "SELECT 1"},
		{"SELECT $$a;|b$$;\nSELECT 2;", "SELECT $$a;b$$"},
	} {
		before := len(db.queries())
		run(p, docAt(c.doc))
		got := db.queries()
		if len(got) != before+1 || got[len(got)-1] != c.want {
			t.Errorf("cursor in %q sent %q, want %q", c.doc, got[before:], c.want)
		}
	}
}

// "run" sends the selection's bytes when there is a selection, whatever the cursor is on.
func TestRunSendsTheSelection(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, _ := connected(t, db)
	text := "SELECT 1;\nSELECT a, b FROM t WHERE a > 1;\n"
	from := strings.Index(text, "a, b")
	to := strings.Index(text, " WHERE")
	d := plugin.Document{Text: text, CursorByte: 0, SelectionBytes: [][2]int{{from, to}}, Version: 1}
	run(p, d)
	if got := db.queries(); len(got) != 1 || got[0] != "a, b FROM t" {
		t.Fatalf("a selection sent %q, want the selected bytes", got)
	}
}

// A cursor inside a comment sends the statement that holds the comment, never the commented text
// as a statement of its own. A note of only comments sends nothing, and the card says so.
func TestACursorInACommentNeverSendsTheCommentedText(t *testing.T) {
	db := newFakeDB(t)
	db.engine = "mysql"
	db.query = whole
	p, h := connected(t, db)

	for _, c := range []struct{ doc, want string }{
		{"SELECT 1 # ; |DROP TABLE t;\n", "SELECT 1 # ; DROP TABLE t;"},
		{"SELECT 1 /*+ ; |DROP TABLE t; */;\n", "SELECT 1 /*+ ; DROP TABLE t; */"},
		{"SELECT 1;\n-- |DROP TABLE t;\nSELECT 2;\n", "SELECT 1"},
	} {
		before := len(db.queries())
		run(p, docAt(c.doc))
		got := db.queries()[before:]
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("cursor in the comment of %q sent %q, want %q", c.doc, got, c.want)
		}
	}
	for _, doc := range []string{"# |DROP TABLE t\n", "/*+ |DROP TABLE t */\n", "-- |DROP TABLE t\n", "|  \n"} {
		before := len(db.queries())
		run(p, docAt(doc))
		if got := db.queries()[before:]; len(got) != 0 {
			t.Errorf("a note of only %q sent %q", doc, got)
		}
		if card := h.card(); !strings.Contains(card, "no statement here") {
			t.Errorf("a note of only %q: the card does not say there is nothing to run:\n%s", doc, card)
		}
	}
	for _, q := range db.queries() {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "DROP") {
			t.Fatalf("commented text was sent as a statement: %q", q)
		}
	}
}

// The statement is AutoDB's: on SQLite the gate reads no trigger bodies, so the cursor on a
// trigger's END finds the END the gate splits off, not the whole trigger golib's SQLite preset
// would read.
func TestTheStatementIsTheGatesNotAPresets(t *testing.T) {
	db := newFakeDB(t)
	db.engine = "sqlite"
	db.query = whole
	p, _ := connected(t, db)
	run(p, docAt("CREATE TRIGGER tr AFTER INSERT ON a BEGIN DELETE FROM b; E|ND;\nSELECT 1;\n"))
	if got := db.queries(); len(got) != 1 || got[0] != "END" {
		t.Fatalf("the cursor on a trigger's END sent %q, want %q, the statement AutoDB's gate reads there", got, "END")
	}
}

// A result with a cursor opens a table named by the cursor; AutoDoc's fetches are answered from
// the cursor until it ends, every row once and in order, and the last page says no more.
func TestPagingHandsTheCursorToTheTable(t *testing.T) {
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) { return page(1, 3, true, "cur-a"), nil }
	next := 4
	db.fetch = func(cur string, n int64) (map[string]any, error) {
		if cur != "cur-a" {
			return nil, &golibrpc.Error{Code: autodb.CodeTxState, Message: "no such cursor"}
		}
		to := min(next+int(n)-1, 10)
		pg := page(next, to, to < 10, "")
		next = to + 1
		return pg, nil
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT n FROM t|"))
	if len(h.tables) != 1 || h.tables[0].ID != "cur-a" || !h.tables[0].More || len(h.tables[0].Rows) != 3 {
		t.Fatalf("the table opened as %+v, want cursor cur-a's first 3 rows with more", h.tables)
	}
	for range 5 {
		if last := h.rows; len(last) > 0 && !last[len(last)-1].more {
			break
		}
		p.FetchRows(plugin.TableFetch{ID: "cur-a", N: 4})
	}
	var all []string
	for _, r := range h.tables[0].Rows {
		all = append(all, r[0])
	}
	for i, sent := range h.rows {
		if sent.id != "cur-a" {
			t.Fatalf("page %d went to %q", i, sent.id)
		}
		for _, r := range sent.rows {
			all = append(all, r[0])
		}
	}
	var want []string
	for i := 1; i <= 10; i++ {
		want = append(want, strconv.Itoa(i))
	}
	if !slices.Equal(all, want) {
		t.Fatalf("the table's rows are %v, want 1 to 10 once each in order", all)
	}
	if last := h.rows[len(h.rows)-1]; last.more {
		t.Fatal("the last page says more rows remain")
	}
	fetched := db.took("exec.session_fetch")
	if len(fetched) == 0 || fetched[0].params[2] != "cur-a" || fetched[0].params[3] != int64(4) {
		t.Fatalf("the fetches were %+v, want cur-a's rows 4 at a time", fetched)
	}
}

// A page is sized by its encoded bytes: rows that would pass the link's bound go in the next
// message, and none is lost.
func TestPagesFitTheLink(t *testing.T) {
	old := maxMessage
	maxMessage = 4 << 10
	t.Cleanup(func() { maxMessage = old })
	big := strings.Repeat("x", 700)
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) {
		rows := []any{}
		for i := range 20 {
			rows = append(rows, []any{strconv.Itoa(i) + big})
		}
		return map[string]any{"verb": "select", "columns": []any{"c"}, "rows": rows, "more": false}, nil
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT c FROM t|"))
	if len(h.tables) != 1 {
		t.Fatalf("tables %+v", h.tables)
	}
	tab := h.tables[0]
	if n, _ := plugin.EncodedSize(plugin.MethodTable, plugin.TableParams(tab)); n > maxMessage {
		t.Fatalf("host.table is %d bytes, past the bound %d", n, maxMessage)
	}
	if !tab.More || len(tab.Rows) >= 20 {
		t.Fatalf("all 20 rows of 700 bytes went in one message of at most %d bytes", maxMessage)
	}
	got := len(tab.Rows)
	for range 20 {
		if len(h.rows) > 0 && !h.rows[len(h.rows)-1].more {
			break
		}
		p.FetchRows(plugin.TableFetch{ID: tab.ID, N: 100})
		last := h.rows[len(h.rows)-1]
		if n, _ := plugin.EncodedSize(plugin.MethodRows, plugin.RowsParams(last.id, last.rows, last.more)); n > maxMessage {
			t.Fatalf("host.rows is %d bytes, past the bound %d", n, maxMessage)
		}
		got += len(last.rows)
	}
	if got != 20 {
		t.Fatalf("%d rows reached the table, want 20", got)
	}
}

// A whole result has no cursor: its table gets an id of its own, new for each result, and says
// no more rows remain.
func TestAWholeResultGetsAFreshTableID(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	run(p, docAt("SELECT 1|"))
	run(p, docAt("SELECT 1|"))
	if len(h.tables) != 2 {
		t.Fatalf("two runs opened %d tables", len(h.tables))
	}
	a, b := h.tables[0], h.tables[1]
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		t.Fatalf("the two whole results' tables are %q and %q: a new table needs a new id", a.ID, b.ID)
	}
	if a.More || b.More {
		t.Fatal("a whole result's table says more rows remain")
	}
}

// The table closed in AutoDoc closes the cursor serving it. A close for a table a newer one has
// replaced closes nothing of the newer one's.
func TestTableClosedClosesTheCursor(t *testing.T) {
	db := newFakeDB(t)
	n := 0
	db.query = func(string) (map[string]any, error) {
		n++
		return page(1, 2, true, "cur-"+strconv.Itoa(n)), nil
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT 1|"))
	run(p, docAt("SELECT 2|"))
	// AutoDoc says the first table is gone once the second replaces it
	p.TableClosed(h.tables[0].ID)
	closed := func() []string {
		var out []string
		for _, c := range db.took("exec.session_cursor_close") {
			out = append(out, c.params[2].(string))
		}
		return out
	}
	if got := closed(); !slices.Equal(got, []string{"cur-1"}) {
		t.Fatalf("after the second run and the first table's close, closed %v, want only cur-1 (closed by the run)", got)
	}
	p.TableClosed("cur-2")
	if got := closed(); !slices.Equal(got, []string{"cur-1", "cur-2"}) {
		t.Fatalf("after the shown table's close, closed %v, want cur-2 too", got)
	}
	p.FetchRows(plugin.TableFetch{ID: "cur-2", N: 10})
	if len(db.took("exec.session_fetch")) != 0 {
		t.Fatal("a fetch for a closed table reached AutoDB")
	}
	if len(h.ends) == 0 || h.ends[len(h.ends)-1].id != "cur-2" {
		t.Fatalf("a fetch for a closed table was not ended: %+v", h.ends)
	}
}

// A token AutoDB no longer takes (a restarted server locks the store) sends the card back to the
// sign-in; signing in again runs.
func TestALockedTokenGoesBackToTheSignIn(t *testing.T) {
	db := newFakeDB(t)
	locked := true
	db.query = func(string) (map[string]any, error) {
		if locked {
			return nil, &golibrpc.Error{Code: autodb.CodeAuth, Message: "auth: store is locked — a passphrase login is required first"}
		}
		return page(1, 1, false, ""), nil
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT 1|"))
	if p.step != stepLogin || p.token != "" {
		t.Fatalf("after a locked store the card is at step %d with a token %t, want the sign-in and none", p.step, p.token != "")
	}
	if card := h.card(); !strings.Contains(card, "Sign in") || !strings.Contains(card, "locked") {
		t.Fatalf("the card does not ask for a sign-in, saying why:\n%s", card)
	}
	locked = false
	signIn(p, "right")
	p.Key(plugin.Key{Key: "Enter"})
	run(p, docAt("SELECT 1|"))
	if len(h.tables) != 1 {
		t.Fatalf("after signing in again the run opened %d tables", len(h.tables))
	}
}

// A sign-in AutoDB refuses stays on the sign-in, saying so; the passphrase typed is not kept.
func TestARefusedSignInStays(t *testing.T) {
	db := newFakeDB(t)
	p := newPlugin("v0.0.1", t.TempDir())
	t.Cleanup(p.Close)
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	h := &fakeHost{}
	p.attach(h, 60, 24)
	signIn(p, "wrong")
	if p.step != stepLogin || p.token != "" {
		t.Fatalf("a refused sign-in left the card at step %d", p.step)
	}
	if len(p.pass) != 0 {
		t.Fatal("the refused passphrase is still held")
	}
	if card := h.card(); !strings.Contains(card, "bad credentials") {
		t.Fatalf("the card does not say the sign-in was refused:\n%s", card)
	}
}

// A fetch AutoDB refuses ends the table with AutoDB's reason; the card says it too.
func TestAFailedFetchEndsTheTableSayingWhy(t *testing.T) {
	db := newFakeDB(t)
	db.query = func(string) (map[string]any, error) { return page(1, 2, true, "cur-f"), nil }
	db.fetch = func(string, int64) (map[string]any, error) {
		return nil, &golibrpc.Error{Code: -32032, Message: "exec: canceling statement due to statement timeout"}
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT n FROM t|"))
	p.FetchRows(plugin.TableFetch{ID: "cur-f", N: 10})
	if len(h.rows) != 0 {
		t.Fatalf("a failed fetch sent rows %+v", h.rows)
	}
	if len(h.ends) != 1 || h.ends[0].id != "cur-f" || !strings.Contains(h.ends[0].why, "statement timeout") {
		t.Fatalf("a failed fetch ended the table as %+v, want cur-f with AutoDB's reason", h.ends)
	}
	if card := h.card(); !strings.Contains(card, "statement timeout") {
		t.Fatalf("the card does not say why the rows ended:\n%s", card)
	}
}

// A note too large to send runs nothing, and the card says so.
func TestANoteTooLargeRunsNothing(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	p.Document(plugin.Document{Path: "big.sql", Version: 3, TooLarge: true})
	p.Command("run")
	p.Command("script")
	if q := db.queries(); len(q) != 0 || len(db.took("exec.run_script")) != 0 {
		t.Fatalf("a note too large to send ran %q", q)
	}
	if card := h.card(); !strings.Contains(card, "too large") {
		t.Fatalf("the card does not say the note is too large:\n%s", card)
	}
}

// "script" runs the whole note through exec.run_script, and shows the last statement's rows.
func TestScriptRunsTheWholeNote(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	text := "CREATE TEMP TABLE x (n int);\nSELECT 1;\n"
	p.Document(plugin.Document{Text: text, Version: 1})
	p.Command("script")
	sc := db.took("exec.run_script")
	if len(sc) != 1 || sc[0].params[2] != text || sc[0].params[1] != int64(7) {
		t.Fatalf("exec.run_script took %+v, want the whole note on connection 7", sc)
	}
	if len(h.tables) != 1 || h.tables[0].More {
		t.Fatalf("the script's last rows: %+v", h.tables)
	}
}
