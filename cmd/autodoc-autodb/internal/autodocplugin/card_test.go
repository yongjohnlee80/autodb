package autodocplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// atSignIn is a plugin opened w×h on db and at its sign-in, sending to h.
func atSignIn(t *testing.T, db *fakeDB, w, hh int) (*Plugin, *fakeHost) {
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
	p.attach(h, w, hh)
	if p.step != stepLogin {
		t.Fatalf("after open the card is at step %d, want the sign-in (%s)", p.step, h.card())
	}
	return p, h
}

// typeText types s into the card, one key a character.
func typeText(p *Plugin, s string) {
	for _, r := range s {
		p.Key(plugin.Key{Key: string(r), Text: string(r)})
	}
}

func tab(p *Plugin)      { p.Key(plugin.Key{Key: "Tab"}) }
func shiftTab(p *Plugin) { p.Key(plugin.Key{Key: "Tab", Shift: true}) }

// a passphrase made for the run, never written down
func freshPass(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "Pp" + hex.EncodeToString(b[:])
}

// rowOf is the index of the first row of lines that satisfies match at or after from, -1 none.
func rowOf(lines []string, from int, match func(string) bool) int {
	for i := max(from, 0); i < len(lines); i++ {
		if match(lines[i]) {
			return i
		}
	}
	return -1
}

func is(s string) func(string) bool  { return func(l string) bool { return l == s } }
func has(s string) func(string) bool { return func(l string) bool { return strings.Contains(l, s) } }
func prefixed(s string) func(string) bool {
	return func(l string) bool { return strings.HasPrefix(l, s) }
}

// The name starts empty, whoever runs AutoDoc: the user's login name is never typed in for them.
func TestTheNameStartsEmpty(t *testing.T) {
	t.Setenv("USER", "someone-else")
	t.Setenv("LOGNAME", "someone-else")
	fresh := New("v0.0.1")
	t.Cleanup(fresh.Close)
	if len(fresh.name) != 0 {
		t.Fatalf("New's name is %q, want empty", string(fresh.name))
	}
	p, h := atSignIn(t, newFakeDB(t), 48, 20)
	if len(p.name) != 0 {
		t.Fatalf("the sign-in opened with the name %q, want empty", string(p.name))
	}
	if card := h.card(); strings.Contains(card, "someone-else") {
		t.Fatalf("the sign-in shows $USER:\n%s", card)
	}
}

// The sign-in is laid out as AutoDoc's own forms: a heading, each field a label over its
// underlined value, the passphrase masked, a rule, Sign in and Cancel, and the keys under them.
// The focused control is the one in the chosen style.
func TestTheSignInIsAForm(t *testing.T) {
	p, h := atSignIn(t, newFakeDB(t), 48, 20)
	pass := freshPass(t)
	typeText(p, "admin")
	tab(p)
	typeText(p, pass)

	if card := h.card(); strings.Contains(card, pass) {
		t.Fatalf("the passphrase shows in clear:\n%s", card)
	}
	lines := h.lines()
	heading := rowOf(lines, 0, is("Sign in to AutoDB"))
	name := rowOf(lines, heading, is("name"))
	passLabel := rowOf(lines, name, is("passphrase"))
	masked := strings.Repeat("•", len(pass)) + "█"
	rule := rowOf(lines, passLabel, prefixed("────"))
	buttons := rowOf(lines, rule, func(l string) bool {
		return strings.Contains(l, "[ Sign in ]") && strings.Contains(l, "[ Cancel ]")
	})
	hint := rowOf(lines, buttons, has("Tab Shift+Tab: move"))
	if heading != 0 || name < 0 || passLabel != name+2 || rule < 0 || buttons < 0 || hint < 0 {
		t.Fatalf("the sign-in's rows are out of the form's order (heading %d, name %d, passphrase %d, rule %d, buttons %d, keys %d):\n%s",
			heading, name, passLabel, rule, buttons, hint, strings.Join(lines, "\n"))
	}
	if lines[name+1] != "admin" {
		t.Errorf("the name field reads %q, want admin", lines[name+1])
	}
	if lines[passLabel+1] != masked {
		t.Errorf("the passphrase field reads %q, want %q: each character masked, the cursor after", lines[passLabel+1], masked)
	}
	if st, _ := h.styleOf(heading, "Sign in to AutoDB"); !st.Bold {
		t.Errorf("the heading is %+v, want bold", st)
	}
	if st, _ := h.styleOf(name+1, "admin"); st != stField {
		t.Errorf("the name field, unfocused, is %+v, want %+v", st, stField)
	}
	if st, _ := h.styleOf(passLabel+1, "•"); st != stFieldFocus {
		t.Errorf("the passphrase field, focused, is %+v, want %+v", st, stFieldFocus)
	}
	for _, b := range []string{"[ Sign in ]", "[ Cancel ]"} {
		if st, _ := h.styleOf(buttons, b); st != stPlain {
			t.Errorf("%s is %+v while a field has the keys, want plain", b, st)
		}
	}

	tab(p) // Sign in
	lines = h.lines()
	passLabel = rowOf(lines, 0, is("passphrase"))
	buttons = rowOf(lines, passLabel, has("[ Sign in ]"))
	hint = rowOf(lines, buttons, has("q: close"))
	if passLabel < 0 || buttons < 0 || hint < 0 {
		t.Fatalf("on Sign in the form lost its passphrase, its buttons or a key line naming q:\n%s", strings.Join(lines, "\n"))
	}
	if st, _ := h.styleOf(buttons, "[ Sign in ]"); st != stChosen {
		t.Errorf("Sign in, focused, is %+v, want %+v", st, stChosen)
	}
	if st, _ := h.styleOf(buttons, "[ Cancel ]"); st != stPlain {
		t.Errorf("Cancel, not focused, is %+v, want plain", st)
	}
	if st, _ := h.styleOf(passLabel+1, "•"); st != stField {
		t.Errorf("the passphrase field, unfocused, is %+v, want %+v", st, stField)
	}
}

// Tab moves through the name, the passphrase, Sign in and Cancel and round again; Shift+Tab goes
// back. Enter on Sign in signs in, and Enter on Cancel asks to close the card, dropping the
// passphrase typed.
func TestTabCyclesTheSignIn(t *testing.T) {
	db := newFakeDB(t)
	p, h := atSignIn(t, db, 48, 20)
	var got []int
	for range 4 {
		tab(p)
		got = append(got, p.field)
	}
	for range 4 {
		shiftTab(p)
		got = append(got, p.field)
	}
	want := []int{focusPass, focusSignIn, focusCancel, focusName, focusCancel, focusSignIn, focusPass, focusName}
	if !slices.Equal(got, want) {
		t.Fatalf("Tab ×4 then Shift+Tab ×4 focused %v, want %v", got, want)
	}

	typeText(p, "admin")
	tab(p)
	typeText(p, "right")
	tab(p)
	if p.field != focusSignIn {
		t.Fatalf("Tab from the passphrase focused %d, want Sign in", p.field)
	}
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepPick || p.token == "" {
		t.Fatalf("Enter on Sign in left the card at step %d with a token %t:\n%s", p.step, p.token != "", h.card())
	}
	if logins := db.took("auth.login"); len(logins) != 1 || logins[0].params[0] != "admin" {
		t.Fatalf("auth.login took %+v, want admin's one sign-in", logins)
	}
	if h.closed() != 0 {
		t.Fatal("signing in asked to close the card")
	}

	p2, h2 := atSignIn(t, db, 48, 20)
	tab(p2)
	typeText(p2, "abc")
	tab(p2)
	tab(p2)
	if p2.field != focusCancel {
		t.Fatalf("Tab ×3 focused %d, want Cancel", p2.field)
	}
	p2.Key(plugin.Key{Key: "Enter"})
	if h2.closed() != 1 {
		t.Fatalf("Enter on Cancel sent host.close %d times, want once", h2.closed())
	}
	if len(p2.pass) != 0 {
		t.Fatal("Cancel left the passphrase typed held")
	}
	if len(db.took("auth.login")) != 1 {
		t.Fatal("Cancel signed in")
	}
}

// q typed in the name or the passphrase is a letter: it is typed, and the card stays open. On a
// button no field has the keys, and q closes.
func TestQInAFieldIsALetter(t *testing.T) {
	p, h := atSignIn(t, newFakeDB(t), 48, 20)
	typeText(p, "q")
	tab(p)
	typeText(p, "q")
	if string(p.name) != "q" || string(p.pass) != "q" {
		t.Fatalf("q in the fields made the name %q and the passphrase %q, want q each", string(p.name), string(p.pass))
	}
	if h.closed() != 0 {
		t.Fatalf("q in a field sent host.close %d times", h.closed())
	}
	if lines := h.lines(); rowOf(lines, 0, is("q")) < 0 || rowOf(lines, 0, is("•█")) < 0 {
		t.Fatalf("the fields do not show the q typed:\n%s", strings.Join(lines, "\n"))
	}
	tab(p) // Sign in
	typeText(p, "q")
	if h.closed() != 1 {
		t.Fatalf("q on Sign in sent host.close %d times, want once", h.closed())
	}
	if len(p.pass) != 0 {
		t.Fatal("closing left the passphrase typed held")
	}
}

// q on the connection pick, the ready state, the failed state and the install offer asks AutoDoc
// to close the card, and the keys say so; the card's state stays for when it is shown again.
func TestQClosesTheCardOffTheFields(t *testing.T) {
	db := newFakeDB(t)
	p, h := atSignIn(t, db, 48, 20)
	signIn(p, "right")
	if p.step != stepPick {
		t.Fatalf("after signing in the card is at step %d", p.step)
	}
	if rowOf(h.lines(), 0, has("q: close")) < 0 {
		t.Fatalf("the pick's keys do not name q:\n%s", strings.Join(h.lines(), "\n"))
	}
	typeText(p, "q")
	if h.closed() != 1 || p.step != stepPick {
		t.Fatalf("q on the pick: host.close %d times, step %d; want once, still the pick", h.closed(), p.step)
	}
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepReady {
		t.Fatalf("Enter on the pick: step %d, want ready", p.step)
	}
	if rowOf(h.lines(), 0, has("q: close")) < 0 {
		t.Fatalf("the ready card's keys do not name q:\n%s", strings.Join(h.lines(), "\n"))
	}
	typeText(p, "q")
	if h.closed() != 2 || p.step != stepReady || p.token == "" || p.sid == "" {
		t.Fatalf("q when ready: host.close %d times, step %d; want twice, still ready and signed in", h.closed(), p.step)
	}

	failed := newPlugin("v0.0.1", t.TempDir())
	t.Cleanup(failed.Close)
	failed.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb"}, nil, nil
	}
	failed.start = func(context.Context, autodb.Binary) (*autodb.Client, error) { return nil, errors.New("no server") }
	fh := &fakeHost{}
	failed.attach(fh, 48, 20)
	typeText(failed, "q")
	if failed.step != stepFailed || fh.closed() != 1 {
		t.Fatalf("q on the failed card: step %d, host.close %d times; want the failed step, once", failed.step, fh.closed())
	}

	install := newPlugin("v0.0.1", t.TempDir())
	t.Cleanup(install.Close)
	install.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{}, []autodb.Tried{{Candidate: autodb.Candidate{Via: "PATH"}, Why: "not found"}}, errors.New("not found")
	}
	installed := false
	install.install = func(context.Context) error { installed = true; return nil }
	ih := &fakeHost{}
	install.attach(ih, 48, 20)
	typeText(install, "q")
	if install.step != stepInstall || ih.closed() != 1 || installed {
		t.Fatalf("q on the install offer: step %d, host.close %d times, installed %t; want the offer, once, nothing installed",
			install.step, ih.closed(), installed)
	}
}

// Hidden, the card keeps its sign-in, its connection and its table, and drops a passphrase half
// typed; shown again, it is drawn as it stands.
func TestHideKeepsTheCardAndShowRedrawsIt(t *testing.T) {
	db := newFakeDB(t)
	p, h := atSignIn(t, db, 48, 20)
	tab(p)
	typeText(p, "half")
	p.Hide()
	if len(p.pass) != 0 {
		t.Fatal("a hidden card holds the passphrase half typed")
	}
	before := h.frames
	p.Show()
	if h.frames != before+1 {
		t.Fatalf("Show sent %d frames, want one", h.frames-before)
	}

	db.query = whole
	c, ch := connected(t, db)
	run(c, docAt("SELECT 1|"))
	c.Hide()
	c.Show()
	if c.step != stepReady || c.token == "" || c.sid == "" || c.table == nil || len(ch.ends) != 0 {
		t.Fatalf("after hide and show: step %d, token %t, session %t, table %t, ends %+v; want all kept",
			c.step, c.token != "", c.sid != "", c.table != nil, ch.ends)
	}
}

// The card is drawn to the size AutoDoc gives it: in a narrow card a long problem wraps at spaces,
// whole words on each row, and the focused field still shows in a short one.
func TestTheCardFitsItsSize(t *testing.T) {
	p, h := atSignIn(t, newFakeDB(t), 24, 20)
	p.problem = "sign-in refused: auth: bad credentials for this user"
	p.draw()
	lines := h.lines()
	for _, l := range lines {
		if n := len([]rune(l)); n > 22 {
			t.Fatalf("a row of %d cells in a card 24 wide (22 inside the margins): %q", n, l)
		}
	}
	got := strings.Join(lines, "\n")
	for _, w := range strings.Fields(p.problem) {
		if !slices.ContainsFunc(lines, func(l string) bool { return slices.Contains(strings.Fields(l), w) }) {
			t.Fatalf("the problem's word %q is cut across rows:\n%s", w, got)
		}
	}

	path := "into /home/me/.local/share/autodoc/plugins/autodb/bin/autodb."
	rows := wrap(path, 22)
	if strings.Join(rows, "") != strings.Replace(path, " ", "", 1) {
		t.Fatalf("wrapping %q lost text: %q", path, rows)
	}
	for _, r := range rows[1 : len(rows)-1] {
		if !strings.HasSuffix(r, "/") {
			t.Fatalf("a path too long for a row is cut inside a name, not after a /: %q", rows)
		}
	}

	if got := wrap("autodb-v0.4.2-linux-amd64.tar.gz", 30); len(got) != 2 || got[0] != "autodb-v0.4.2-linux-" {
		t.Fatalf("a file name too long for a row is cut at %q, want after its last - that fits", got)
	}

	short, sh := atSignIn(t, newFakeDB(t), 48, 9)
	short.noUsers = true
	tab(short)
	typeText(short, "x")
	lines = sh.lines()
	if lines[0] != "Sign in to AutoDB" || rowOf(lines, 0, is("•█")) < 0 {
		t.Fatalf("a short card lost its heading or its focused field:\n%s", strings.Join(lines, "\n"))
	}
}

// The connection picked is named everywhere once it is picked: the card's title, the first row of
// every step (the ready card, a query running, an error, the pick itself, where its row is marked
// current). Before a pick the title is AutoDB alone and no row names a connection.
func TestTheConnectionShowsAfterAPick(t *testing.T) {
	db := newFakeDB(t)
	p, h := atSignIn(t, db, 48, 20)
	if h.title() != "AutoDB" {
		t.Fatalf("the sign-in's title is %q, want AutoDB", h.title())
	}
	signIn(p, "right")
	if h.title() != "AutoDB" || h.lines()[0] != "Choose a connection" {
		t.Fatalf("before a pick: title %q, first row %q; want AutoDB and the heading", h.title(), h.lines()[0])
	}
	p.Key(plugin.Key{Key: "Enter"})
	const header = "Connection: scratch (postgres)"
	if h.title() != "AutoDB · scratch (postgres)" {
		t.Fatalf("after the pick the title is %q", h.title())
	}
	if lines := h.lines(); lines[0] != header || lines[1] != "Ready" {
		t.Fatalf("the ready card's first rows are %q, want the connection then the heading", lines[:2])
	}
	if st, _ := h.styleOf(0, "Connection"); st != stGood {
		t.Fatalf("the connection's row is %+v, want %+v", st, stGood)
	}

	seen := make(chan string, 1)
	db.query = func(string) (map[string]any, error) {
		seen <- h.lines()[0]
		return nil, &golibrpc.Error{Code: -32032, Message: "exec: relation \"nope\" does not exist"}
	}
	run(p, docAt("SELECT * FROM nope|"))
	if running := <-seen; running != header {
		t.Fatalf("while the query ran the first row was %q, want %q", running, header)
	}
	if lines := h.lines(); lines[0] != header || !strings.Contains(h.card(), "does not exist") {
		t.Fatalf("after a failed statement the card is:\n%s\nwant the connection first and the error", strings.Join(lines, "\n"))
	}

	p.Key(plugin.Key{Key: "c"})
	if lines := h.lines(); lines[0] != header || rowOf(lines, 0, is("> scratch (postgres) · current")) < 0 {
		t.Fatalf("the pick does not name and mark the connection in use:\n%s", strings.Join(lines, "\n"))
	}
}

// Picking another connection renames the title and the first row, with the workspace conn.list
// names it under, and the pick marks the new one current.
func TestPickingAnotherConnectionRenamesTheCard(t *testing.T) {
	db := newFakeDB(t)
	db.more = []map[string]any{{"id": int64(8), "name": "ledger", "engine": "sqlite", "workspace": "finance"}}
	p, h := connected(t, db)
	if h.title() != "AutoDB · scratch (postgres)" {
		t.Fatalf("the title is %q", h.title())
	}
	p.Key(plugin.Key{Key: "c"})
	lines := h.lines()
	if rowOf(lines, 0, is("> scratch (postgres) · current")) < 0 || rowOf(lines, 0, is("ledger (sqlite)")) < 0 {
		t.Fatalf("the pick before the change:\n%s", strings.Join(lines, "\n"))
	}
	p.Key(plugin.Key{Key: "j"})
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepReady || p.conn == nil || p.conn.ID != 8 {
		t.Fatalf("after choosing ledger: step %d, connection %+v", p.step, p.conn)
	}
	if h.title() != "AutoDB · ledger (sqlite)" {
		t.Fatalf("after the change the title is %q", h.title())
	}
	if l := h.lines()[0]; l != "Connection: ledger (sqlite) · finance" {
		t.Fatalf("after the change the first row is %q", l)
	}
	p.Key(plugin.Key{Key: "c"})
	lines = h.lines()
	if rowOf(lines, 0, is("scratch (postgres)")) < 0 || rowOf(lines, 0, is("> ledger (sqlite) · current")) < 0 {
		t.Fatalf("the pick after the change does not mark ledger current:\n%s", strings.Join(lines, "\n"))
	}
}

// The results table is titled with the connection it came from, a whole result and a cursor's.
func TestTheResultsTitleNamesTheConnection(t *testing.T) {
	db := newFakeDB(t)
	paged := false
	db.query = func(string) (map[string]any, error) {
		if paged {
			return page(1, 2, true, "cur-t"), nil
		}
		return page(1, 1, false, ""), nil
	}
	p, h := connected(t, db)
	run(p, docAt("SELECT 1|"))
	paged = true
	run(p, docAt("SELECT n FROM t|"))
	if len(h.tables) != 2 || h.tables[1].ID != "cur-t" {
		t.Fatalf("the runs opened %+v, want a whole table then cur-t's", h.tables)
	}
	for _, tab := range h.tables {
		if tab.Title != "Results · scratch" {
			t.Errorf("table %s is titled %q, want %q", tab.ID, tab.Title, "Results · scratch")
		}
	}
}

// A connection lost, or a sign-in AutoDB no longer takes, keeps the last connection's name in the
// title and the first row, saying it is not connected.
func TestALostConnectionIsNamedNotConnected(t *testing.T) {
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	_ = p.cli.Close()
	run(p, docAt("SELECT 1|"))
	if p.step != stepFailed {
		t.Fatalf("after the lost connection the card is at step %d", p.step)
	}
	if h.title() != "AutoDB · scratch · not connected" {
		t.Fatalf("after the lost connection the title is %q", h.title())
	}
	if l := h.lines()[0]; l != "Connection: scratch (postgres) · not connected" {
		t.Fatalf("after the lost connection the first row is %q", l)
	}

	db2 := newFakeDB(t)
	db2.query = func(string) (map[string]any, error) {
		return nil, &golibrpc.Error{Code: autodb.CodeAuth, Message: "auth: store is locked"}
	}
	q, qh := connected(t, db2)
	run(q, docAt("SELECT 1|"))
	if q.step != stepLogin || qh.title() != "AutoDB · scratch · not connected" {
		t.Fatalf("after a sign-in refused: step %d, title %q", q.step, qh.title())
	}
}

// fits fails the cell when a row of the last frame passes the card's width inside its margins, or
// cuts a word of want across rows.
func fits(t *testing.T, h *fakeHost, w int, state string, want ...string) {
	t.Helper()
	lines := h.lines()
	for _, l := range lines {
		if n := len([]rune(l)); n > w-2 {
			t.Errorf("%s: a row of %d cells in a card %d wide: %q", state, n, w, l)
		}
	}
	for _, s := range want {
		for _, word := range strings.Fields(s) {
			if !slices.ContainsFunc(lines, func(l string) bool { return slices.Contains(strings.Fields(l), word) }) {
				t.Errorf("%s: %q is cut across rows:\n%s", state, word, strings.Join(lines, "\n"))
			}
		}
	}
}

// Docked on the right, the card is a narrow column: at 32 columns the pick, the ready card, the
// failed card and the install offer fit, their words whole, the connection still first.
func TestTheCardReadsInANarrowColumn(t *testing.T) {
	const w = 32
	db := newFakeDB(t)
	db.query = whole
	p, h := connected(t, db)
	p.Resize(w, 24)
	fits(t, h, w, "ready", "Connection: scratch (postgres)", "c: another connection · q: close")
	if l := h.lines()[0]; l != "Connection: scratch (postgres)" {
		t.Errorf("ready at %d: the first row is %q", w, l)
	}
	p.Key(plugin.Key{Key: "c"})
	fits(t, h, w, "pick", "↑↓ j k: choose · Enter: connect · q: close")
	if rowOf(h.lines(), 0, is("> scratch (postgres) · current")) < 0 {
		t.Errorf("pick at %d:\n%s", w, strings.Join(h.lines(), "\n"))
	}
	p.Key(plugin.Key{Key: "Enter"})
	_ = p.cli.Close()
	run(p, docAt("SELECT 1|"))
	fits(t, h, w, "failed", "Enter: try again · q: close", p.problem)
	if l := h.lines()[0]; !strings.HasPrefix(l, "Connection: scratch") {
		t.Errorf("failed at %d: the first row is %q", w, l)
	}

	in := newPlugin("v0.4.2", t.TempDir())
	t.Cleanup(in.Close)
	in.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{}, []autodb.Tried{{Candidate: autodb.Candidate{Via: "PATH"}, Why: "no autodb on PATH"}}, errors.New("not found")
	}
	ih := &fakeHost{}
	in.attach(ih, w, 30)
	fits(t, ih, w, "install", "AutoDB was not found. Looked in:", "i: install · q: close")
}

// A pick longer than a short card scrolls under the connection and the heading, which stay, so the
// chosen row shows.
func TestALongPickScrollsUnderTheConnection(t *testing.T) {
	db := newFakeDB(t)
	for i := range 12 {
		db.more = append(db.more, map[string]any{"id": int64(100 + i), "name": "db" + string(rune('a'+i)), "engine": "sqlite"})
	}
	p, h := connected(t, db)
	p.Key(plugin.Key{Key: "c"})
	p.Resize(32, 10)
	for range 12 {
		p.Key(plugin.Key{Key: "j"})
	}
	lines := h.lines()
	if lines[0] != "Connection: scratch (postgres)" || lines[1] != "Choose a connection" || rowOf(lines, 0, is("> dbl (sqlite)")) < 0 {
		t.Fatalf("a long pick in a short card lost the connection, the heading or the chosen row:\n%s", strings.Join(lines, "\n"))
	}
}
