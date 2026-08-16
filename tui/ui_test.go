package tui_test

// Headless end-to-end drive of the FULL TUI against a REAL server: boot,
// bootstrap the root user through the form, create a connection through
// the manager float, run a query with the vim editor, and read the results
// off the virtual grid. This is the M6 exit gate in test form.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// startRealServer boots a full autodb server on a loopback port.
func startRealServer(t *testing.T) (addr string) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "e2e", rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	for srv.Addr() == "" {
		time.Sleep(time.Millisecond)
	}
	return srv.Addr()
}

type uiHarness struct {
	t         *testing.T
	tb        *tuicore.TestBackend
	app       *tuicore.App
	done      chan error
	notesRoot string
	trace     *traceLog
}

// traceLog buffers runtime trace records for failure diagnostics.
type traceLog struct {
	mu   sync.Mutex
	recs []string
}

func (l *traceLog) add(ev tuicore.TraceEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := ev.Kind.String() + " node=" + ev.Comp
	if ev.PrevComp != "" {
		line += " prev=" + ev.PrevComp
	}
	if ev.Detail != "" {
		line += " (" + ev.Detail + ")"
	}
	l.recs = append(l.recs, line)
}

// tail returns the last n records, newest last.
func (l *traceLog) tail(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	from := max(len(l.recs)-n, 0)
	return strings.Join(l.recs[from:], "\n")
}

func startUI(t *testing.T, addr string) *uiHarness {
	t.Helper()
	notesRoot := filepath.Join(t.TempDir(), "notes")
	notes, err := tuiapp.NewNoteStore(notesRoot)
	if err != nil {
		t.Fatal(err)
	}
	session := tuiapp.NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(session.Close)

	ctx, cancel := context.WithCancel(context.Background())
	model := tuiapp.New(session, notes, cancel)
	tb := tuicore.NewTestBackend(110, 32)
	// Runtime tracing: interactive failures are timing failures, and the
	// trace is the evidence (who has focus, what consumed the key).
	tr := &traceLog{}
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(tb),
		tuicore.WithMinFrameInterval(0), tuicore.WithTrace(tr.add))

	h := &uiHarness{t: t, tb: tb, app: app, done: make(chan error, 1),
		notesRoot: notesRoot, trace: tr}
	go func() { h.done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil && err != context.Canceled {
				t.Errorf("app.Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("app never exited")
		}
	})
	return h
}

func (h *uiHarness) screen() string { return h.tb.String() }

// waitFor polls the virtual screen for a substring.
func (h *uiHarness) waitFor(what, sub string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.screen(), sub) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("waiting for %s: %q never appeared.\nscreen:\n%s\n\nlast runtime trace:\n%s",
		what, sub, h.screen(), h.trace.tail(40))
}

// waitGone polls until a substring DISAPPEARS from the virtual screen.
func (h *uiHarness) waitGone(what, sub string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(h.screen(), sub) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("waiting for %s to disappear: %q still shown.\nscreen:\n%s", what, sub, h.screen())
}

// leader invokes a leader binding and returns only once the menu has
// closed. Leader labels intentionally mirror the titles of the floats
// they open ("script history…" / "script history"), so asserting right
// after the keypress can match the MENU and race ahead of the float.
func (h *uiHarness) leader(binding string) {
	h.t.Helper()
	h.key(' ')
	h.waitFor("leader menu", "SPC — commands")
	h.keys(binding)
	h.waitGone("leader menu", "SPC — commands")
}

func (h *uiHarness) keys(s string) {
	for _, r := range s {
		ev := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: r, Text: string(r)}
		if err := h.tb.Inject(ev); err != nil {
			h.t.Fatal(err)
		}
	}
}

// ctrl injects a Ctrl-modified key (pane motion).
func (h *uiHarness) ctrl(code rune) {
	if err := h.tb.Inject(tuicore.KeyEvent{
		Kind: tuicore.KeyPress, Code: code, Mods: tuicore.ModCtrl,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *uiHarness) key(code rune) {
	text := ""
	if code >= 0x20 && code < 0xE000 {
		text = string(code)
	}
	if err := h.tb.Inject(tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: code, Text: text}); err != nil {
		h.t.Fatal(err)
	}
}

// explorerCursorBG returns the background color index of the explorer's
// highlighted row, or false when no row is filled.
func (h *uiHarness) explorerCursorBG() (uint8, bool) {
	cells := h.tb.Snapshot()
	if len(cells) < 3 {
		return 0, false
	}
	for y := 1; y < len(cells)-1; y++ {
		a := cells[y][2].Attrs
		if a.BG.Kind == 0 || a.BG.Index == 0 {
			continue
		}
		if cells[y][3].Attrs == a && cells[y][4].Attrs == a {
			return a.BG.Index, true
		}
	}
	return 0, false
}

// waitCursorBG polls until the explorer's highlight uses the wanted
// background (styling lands on the focus event, one loop turn later).
func (h *uiHarness) waitCursorBG(what string, want uint8) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last uint8
	for time.Now().Before(deadline) {
		if bg, ok := h.explorerCursorBG(); ok {
			if bg == want {
				return
			}
			last = bg
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("%s: explorer highlight background = %d, want %d\nscreen:\n%s",
		what, last, want, h.screen())
}

func TestUIFullFlow(t *testing.T) {
	addr := startRealServer(t)
	h := startUI(t, addr)

	// 1. First run: the bootstrap form appears (master passphrase setup).
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	// 2. Create a connection through the manager float (SPC c → a).
	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.keys("a")
	h.waitFor("connection form", "new connection")
	h.keys("demo")
	h.key(tuicore.KeyTab)
	h.keys("sqlite")
	h.key(tuicore.KeyTab)
	h.keys(fmt.Sprintf("file:uie2e%d?mode=memory&cache=shared", time.Now().UnixNano()))
	h.key(tuicore.KeyEnter)
	// The submit closes the form — wait for that FIRST so the row
	// assertions below can only match the reloaded manager table (the
	// form held the same strings).
	h.waitGone("connection form", "new connection")
	h.waitFor("created connection row", "demo")
	h.waitFor("created connection engine", "sqlite")
	h.key(tuicore.KeyEscape) // close the manager

	// 2b. The users manager shows its full key list (it was truncated at
	//     "g:gr…" before the float widened and the footer wrapped).
	h.leader("u")
	h.waitFor("users manager", "a:add")
	h.waitFor("full key list", "g:grant on conn")
	h.waitFor("reset key", "p:reset passphrase")
	h.key(tuicore.KeyEscape)

	// 3. Create a workspace and attach the connection (server-side ids 1).
	h.leader("w")
	h.waitFor("workspace manager", "a:add")
	h.keys("a")
	h.waitFor("workspace form", "new workspace")
	h.keys("main")
	h.key(tuicore.KeyEnter)
	h.waitGone("workspace form", "new workspace")
	h.waitFor("workspace row", "main")
	h.key(tuicore.KeyEscape)

	h.leader("c")
	h.waitFor("connections manager", "a:add")
	h.waitFor("connection row loaded", "demo") // rows land async; 'w' needs a selection
	h.keys("w")                                // attach selected conn to a workspace
	h.waitFor("attach form", "attach demo to workspace")
	h.keys("1")
	h.key(tuicore.KeyEnter)
	h.waitGone("attach form", "attach demo to workspace")
	time.Sleep(300 * time.Millisecond) // let the attach round-trip settle
	h.key(tuicore.KeyEscape)

	// 4. The explorer shows the workspace; walk to the connection so it
	//    becomes the active one.
	h.waitFor("explorer", "main")
	h.key(0x03) // no-op guard; keep event flow moving
	// Focus the explorer pane and navigate: ws → connections → conn.
	h.leader("e")
	h.keys("l")  // expand ws (pre-assembled: shows connections/notes)
	h.keys("jl") // onto "connections", expand (already loaded)
	h.keys("j")  // onto the demo connection
	h.waitFor("active connection", "demo")

	// 5. Write a query with the vim editor and run it via the leader.
	h.leader("q") // focus query editor
	h.keys("i")   // insert mode
	h.keys("CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT NOT NULL)")
	h.keys("jk") // escape chord
	h.leader("r")
	h.waitFor("DDL ack", "CREATE ok")

	h.keys("ggVG") // select-all in the editor… then replace via insert
	h.key(tuicore.KeyEscape)
	h.keys("gg")
	h.keys("V")
	h.keys("G")
	h.keys("d") // delete the old buffer content
	h.keys("i")
	h.keys("INSERT INTO songs (title) VALUES ('alpha'), ('beta')")
	h.keys("jk")
	h.leader("r")
	h.waitFor("insert ack", "INSERT ok — 2 row(s) affected")

	h.keys("Vd") // clear the single line
	h.keys("i")
	h.keys("SELECT id, title FROM songs ORDER BY id")
	h.keys("jk")
	h.leader("r")
	h.waitFor("result rows", "alpha")
	h.waitFor("result rows", "beta")
	h.waitFor("result summary", "SELECT ok — 2 row(s)")

	// 5b. JSON toggle renders the same rows as a JSON document (the
	//     BufferView must be mounted BEFORE its writer is fed).
	h.leader("j")
	h.waitFor("json document", "\"title\": \"alpha\"")
	// The JSON view is a read-only vim VIEWER: motions and yank work,
	// edits are refused.
	h.ctrl('j') // focus results
	h.keys("jj")
	h.keys("Vy") // visual-line yank into the editor register
	h.keys("ix") // insert would mutate — refused
	h.waitFor("json intact", "\"title\": \"alpha\"")
	h.leader("j")
	h.waitFor("back to table", "beta")

	// 5c. Directional pane motion: Ctrl-j moves DOWN into the results
	//     (proved by `v` opening the row inspector there), Ctrl-k moves
	//     back UP to the query editor.
	h.ctrl('j')
	h.keys("j") // vim motion inside the results table (golib List)
	h.keys("v")
	h.waitFor("results focused", "y: copy value")
	h.waitFor("second row selected", "beta") // j moved the cursor before v
	h.key(tuicore.KeyEscape)
	h.ctrl('k')

	// 5d. Ctrl-h reaches the explorer, and Ctrl-l comes back out of it —
	//     the tree must NOT read those chords as its own h/l collapse and
	//     expand (golib bubbles Ctrl-modified keys).
	h.ctrl('h')
	h.ctrl('l')
	h.keys("i") // insert mode only lands if the editor took focus
	h.keys("-- back in the editor")
	h.keys("jk")
	h.waitFor("editor focused after Ctrl-l", "-- back in the editor")
	h.keys("Vd") // clear that probe line

	// 5e. In-panel search: / prompts, n / N walk matches — in each panel.
	//     Results first (two rows, one match each).
	h.ctrl('j')
	h.keys("/")
	h.waitFor("search prompt", "search in results")
	h.keys("beta")
	h.key(tuicore.KeyEnter)
	h.waitFor("results match", "beta: match 1/1 in the results")

	// The explorer searches the VISIBLE node labels (a collapsed subtree
	// is not on screen, so it is not searchable — same as the eye sees).
	h.ctrl('h')
	h.keys("/")
	h.waitFor("search prompt", "search in explorer")
	h.keys("notes")
	h.key(tuicore.KeyEnter)
	h.waitFor("explorer match", "notes: match 1/1 in the explorer")
	h.keys("/")
	h.keys("songs") // collapsed away → honestly reported as no match
	h.key(tuicore.KeyEnter)
	h.waitFor("explorer miss", "no match for songs in the explorer")

	// The query editor searches its own lines (the buffer was emptied by
	// the pane-motion probe above, so give it content first).
	h.ctrl('l')
	h.keys("i")
	h.keys("SELECT id FROM songs")
	h.keys("jk")
	h.keys("/")
	h.waitFor("search prompt", "search in query")
	h.keys("songs")
	h.key(tuicore.KeyEnter)
	h.waitFor("editor match", "songs: match 1/1 in the query")
	h.keys("n") // single match: n wraps back onto it
	h.waitFor("editor wrap", "songs: match 1/1 in the query")

	// 5f. The cursor highlight follows FOCUS: the explorer's row is
	//     styled one way while it holds focus and another once focus
	//     moves to the query editor — and the change lands on the focus
	//     event itself, without waiting for some later re-layout.
	const cyan, gray = 6, 8
	h.ctrl('h')
	h.waitCursorBG("explorer focused", cyan)
	h.ctrl('l')
	h.waitCursorBG("explorer blurred by moving to the query editor", gray)
	h.ctrl('h')
	h.waitCursorBG("explorer refocused", cyan)
	h.ctrl('l')

	// 5g. TWO statements in one buffer: both run, the LAST result shows.
	h.keys("Vd")
	h.keys("i")
	h.keys("INSERT INTO songs (title) VALUES ('gamma'); SELECT title FROM songs ORDER BY id")
	h.keys("jk")
	h.leader("r")
	h.waitFor("script summary", "2 statements")
	h.waitFor("last result shown", "gamma")

	// 6. The WHERE-less guard surfaces as a structured status message.
	h.keys("Vd")
	h.keys("i")
	h.keys("UPDATE songs SET title = 'x'")
	h.keys("jk")
	h.leader("r")
	h.waitFor("guard refusal", "WHERE clause is blocked")

	// 6b. SPC s with NO note open saves the BUFFER under a new name — it
	//     used to create the note and load the empty file over the text,
	//     losing it.
	h.keys("Vd")
	h.keys("i")
	h.keys("-- unsaved buffer")
	h.keys("jk")
	h.leader("s")
	h.waitFor("save-as prompt", "save note as")
	h.keys("frombuffer")
	h.key(tuicore.KeyEnter)
	h.waitFor("buffer saved", "saved frombuffer.sql")
	if body, err := os.ReadFile(filepath.Join(h.notesRoot, "ws-1", "frombuffer.sql")); err != nil {
		t.Fatalf("saved note: %v", err)
	} else if string(body) != "-- unsaved buffer" {
		t.Fatalf("SPC s wrote %q, want the editor buffer", string(body))
	}

	// 6b-2. The saved note appears in the explorer WITHOUT a manual
	//       refresh, and `d` deletes it (confirmed).
	h.ctrl('h')
	h.keys("g")
	h.keys("jjj") // workspace → connections → demo → notes
	h.keys("l")   // expand the notes folder
	// Anchor on the TREE row (the leaf bullet): the bare filename also
	// appears in the status bar, which would pass this wait early.
	h.waitFor("saved note listed", "· frombuffer.sql")
	h.keys("j") // onto the note
	h.keys("d")
	h.waitFor("delete confirm", "delete frombuffer.sql")
	h.keys("y")
	h.waitFor("deleted", "deleted frombuffer.sql")
	h.waitGone("note gone from the explorer", "· frombuffer.sql")
	h.ctrl('l')

	// 6c. The explorer's `a` adds a note under a workspace's notes folder.
	h.ctrl('h')
	h.keys("g")   // first row: the workspace
	h.keys("jjj") // connections → demo → notes
	h.keys("a")
	h.waitFor("note form from explorer", "new note")
	h.keys("fromexplorer")
	h.key(tuicore.KeyEnter)
	// The FILE exists as soon as it is named, so the explorer shows it
	// without waiting for a first save.
	h.waitFor("created note listed", "· fromexplorer.sql")
	if _, err := os.Stat(filepath.Join(h.notesRoot, "ws-1", "fromexplorer.sql")); err != nil {
		t.Fatalf("note created from the explorer is not on disk: %v", err)
	}
	h.ctrl('l')

	// 6d. SPC C picks the query connection from a list, and the query
	//     panel names the target.
	h.leader("C")
	// Wait for content ONLY the picker renders. Waiting on its title used
	// to pass against the leader menu's identically-worded entry, so the
	// test pressed Enter before the float existed — the runtime trace
	// showed the key consumed by nobody, then the float opening.
	h.waitFor("connection picker", "(sqlite, id 1)")
	h.key(tuicore.KeyEnter)
	h.waitFor("target adopted", "query connection: demo")
	h.waitFor("query panel names the target", "query → demo")

	// 6e. Script history: every execution above was recorded — who ran
	//     what, when. The script is ellipsed in the table and Enter opens
	//     it in full.
	h.leader("H")
	h.waitFor("history float", "script history")
	h.waitFor("history records the user", "root")
	h.waitFor("history records the connection", "demo")
	h.key(tuicore.KeyEnter)
	// Which row is newest depends on millisecond-resolution timestamps,
	// so assert what IS guaranteed: the detail float opened, titled with
	// the run's identity, showing one of this session's scripts.
	h.waitFor("detail float titled", "· root ·")
	h.waitFor("full script shown", "songs")
	h.key(tuicore.KeyEscape)
	h.key(tuicore.KeyEscape)

	// 7. Notes: create, save, external clobber → conflict float → save-as
	//    preserves the EDITED body under the new name (no data loss).
	h.leader("n")
	h.waitFor("note form", "new note")
	h.keys("scratch")
	h.key(tuicore.KeyEnter)
	h.waitFor("note open", "scratch.sql")
	h.keys("i")
	h.keys("-- v1")
	h.keys("jk")
	h.leader("s")
	h.waitFor("note saved", "saved scratch.sql")

	notePath := filepath.Join(h.notesRoot, "ws-1", "scratch.sql")
	if err := os.WriteFile(notePath, []byte("-- external edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys("A")
	h.keys(" more")
	h.keys("jk")
	h.leader("s")
	h.waitFor("conflict float", "save as a new name")
	h.keys("s")
	h.waitFor("save-as form", "save note as")
	h.keys("scratch2")
	h.key(tuicore.KeyEnter)
	h.waitFor("save-as done", "saved scratch2.sql")
	saved, err := os.ReadFile(filepath.Join(h.notesRoot, "ws-1", "scratch2.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(saved); got != "-- v1 more" {
		t.Fatalf("save-as body = %q, want the edited buffer", got)
	}
	if ext, _ := os.ReadFile(notePath); string(ext) != "-- external edit\n" {
		t.Fatalf("original note clobbered: %q", string(ext))
	}

	// 8. `?` shows the CONTEXT keys in the bottom-right corner (the full
	//    leader table lives behind SPC ?).
	h.key('?')
	h.waitFor("context keys", "query editor — keys")
	h.waitFor("context binding", "run the query")
	h.key(tuicore.KeyEscape) // any key dismisses the card
	h.leader("?")
	h.waitFor("leader help", "leader commands")
	h.key(tuicore.KeyEscape)

	// 9. Explorer drill-down to columns, then Enter on the table scaffolds
	//    from the server-quoted identifier.
	h.leader("e")
	// Start from a known row: g jumps to the first (the workspace), then
	// down onto the connection — the search above left the cursor
	// wherever its match was.
	h.keys("g")
	h.keys("jj") // → connections → the demo connection
	h.keys("l")  // expand it → schemas
	h.waitFor("schema listed", "▸ main")
	h.keys("jl") // onto schema "main", expand → sections
	h.waitFor("sections", "views")
	h.keys("jl") // onto "tables", expand → songs
	h.waitFor("table listed", "songs")
	h.keys("jl")                              // onto songs, expand → columns
	h.waitFor("columns listed", "title TEXT") // badge ellipsized by pane width
	h.key(tuicore.KeyEnter)                   // Enter on the TABLE still scaffolds
	h.waitFor("scaffold", "LIMIT 100")

	// 10. Disconnect / reconnect toggle (leader x); the token survives a
	//     same-instance reconnect.
	h.leader("x")
	h.waitFor("disconnected", "disconnected — SPC x reconnects")
	h.leader("x")
	h.waitFor("reconnected", "logged in as root")
}
