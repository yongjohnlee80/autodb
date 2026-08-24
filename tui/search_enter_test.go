package tui

// Item 3b: Johno reports that after searching with `/`, ENTER expands the node's
// columns instead of loading the table scaffold — in BOTH frontends, so it is in
// the shared model rather than the gateway.
//
// The ticket already ruled out two theories by reading: the search and tree
// index spaces DO agree (VisibleRows is flatten mapped, Cursor is documented as
// an index into it), and the explorer DOES see ENTER before forwarding to the
// tree. This drives the real thing instead.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tui "github.com/yongjohnlee80/golib/tui"
)

func TestSearchThenEnter_LoadsTheScaffoldNotTheColumns(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "search-passphrase-1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	b := sess.Bind()
	cid, err := b.CreateConnection(ctx, "bravo", "sqlite", filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, cid, `CREATE TABLE data_schema (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	wsID, err := b.CreateWorkspace(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AttachConnection(ctx, wsID, cid); err != nil {
		t.Fatal(err)
	}

	m, tb, sync := mountedWith(t, sess)
	e := m.explorer
	sync(func() { e.Reload() })

	ws := "ws:" + strconv.FormatInt(wsID, 10)
	conns := childWithPrefix(t, sync, e, []string{ws}, "conns:")
	conn := childWithPrefix(t, sync, e, []string{ws, conns}, "conn:")
	schema := childWithPrefix(t, sync, e, []string{ws, conns, conn}, "schema:")
	sec := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema}, "sec:")
	tbl := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema, sec}, "tbl:")

	// Close the startup splash through the real key path; modalOpen is the precise
	// signal, unlike matching rendered text.
	dl := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl) {
		var open bool
		sync(func() { open = m.modalOpen() })
		if !open {
			break
		}
		_ = tb.Inject(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
		time.Sleep(30 * time.Millisecond)
	}
	var modal bool
	sync(func() { modal = m.modalOpen() })
	if modal {
		t.Fatalf("startup modal never closed:\n%s", tb.String())
	}

	// Focus the explorer and park the cursor OFF the table.
	sync(func() {
		m.focusPane(m.explorer)
		e.tree.SetCursor(0)
		m.editor.SetValue("")
	})
	var startID string
	sync(func() {
		if n, ok := e.tree.Selected(); ok {
			startID = n.ID()
		}
	})
	t.Logf("cursor starts on %q (target is %q)", startID, tbl)

	inject := func(evs ...tui.Event) {
		for _, ev := range evs {
			if err := tb.Inject(ev); err != nil {
				t.Fatal(err)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	typeStr := func(s string) {
		for _, r := range s {
			inject(tui.KeyEvent{Kind: tui.KeyPress, Code: r, Text: string(r)})
		}
	}

	// `/` opens the search form.
	inject(tui.KeyEvent{Kind: tui.KeyPress, Code: '/', Text: "/"})
	dl = time.Now().Add(5 * time.Second)
	for time.Now().Before(dl) {
		if strings.Contains(tb.String(), "search in explorer") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(tb.String(), "search in explorer") {
		t.Fatalf("`/` did not open the search prompt:\n%s", tb.String())
	}

	typeStr("data_schema")
	inject(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter}) // submit
	time.Sleep(150 * time.Millisecond)

	var afterSubmit, editorAfterSubmit string
	var expandedAfterSubmit, modalAfterSubmit bool
	sync(func() {
		if n, ok := e.tree.Selected(); ok {
			afterSubmit = n.ID()
			expandedAfterSubmit = len(e.tree.VisibleRows()) > 6 // columns would appear
		}
		editorAfterSubmit = m.editor.Value()
		modalAfterSubmit = m.modalOpen()
	})
	t.Logf("after submit: selected=%q modalOpen=%v editor=%q rows>6=%v",
		afterSubmit, modalAfterSubmit, editorAfterSubmit, expandedAfterSubmit)

	// The SECOND Enter is the one Johno describes: on the found table it should
	// scaffold, not expand.
	inject(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
	time.Sleep(200 * time.Millisecond)

	var editorAfter, selAfter string
	var rows int
	sync(func() {
		editorAfter = m.editor.Value()
		if n, ok := e.tree.Selected(); ok {
			selAfter = n.ID()
		}
		rows = len(e.tree.VisibleRows())
	})
	t.Logf("after 2nd Enter: selected=%q rows=%d editor=%q", selAfter, rows, editorAfter)

	if afterSubmit != tbl {
		t.Errorf("search did not land on the table: selected %q, want %q", afterSubmit, tbl)
	}
	if !strings.Contains(editorAfter, "SELECT * FROM") {
		t.Errorf("Enter on the found table did not scaffold; editor = %q. "+
			"If the visible row count grew, the tree expanded the columns instead.", editorAfter)
	}
}

// THE REPRODUCTION. widget.Tree.AcceptsFocus() is true, and golib's
// focusFromPointer walks OUTWARD from the click target taking the FIRST
// focusable — so clicking in the explorer focuses the *Tree*, not the *explorer*
// wrapper that owns the scaffold intercept.
//
// The explorer's Enter handling only runs because the explorer forwards to the
// tree (`return e.tree.HandleEvent(ev)`). With focus ON the tree, the tree gets
// Enter first, consumes it as expand/collapse, and the intercept never runs — so
// a table opens its COLUMNS instead of scaffolding. That is item 3b, and it is
// reachable only after a pointer click, which is why it appeared alongside the
// v0.5.2 click-to-focus work and in both frontends.
func TestEnterWithFocusOnTheTree_StillScaffolds(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "treefocus-passphrase-1"); err != nil {
		t.Fatal(err)
	}
	b := sess.Bind()
	cid, err := b.CreateConnection(ctx, "bravo", "sqlite", filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, cid, `CREATE TABLE data_schema (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	wsID, err := b.CreateWorkspace(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AttachConnection(ctx, wsID, cid); err != nil {
		t.Fatal(err)
	}

	m, tb, sync := mountedWith(t, sess)
	e := m.explorer
	sync(func() { e.Reload() })

	ws := "ws:" + strconv.FormatInt(wsID, 10)
	conns := childWithPrefix(t, sync, e, []string{ws}, "conns:")
	conn := childWithPrefix(t, sync, e, []string{ws, conns}, "conn:")
	schema := childWithPrefix(t, sync, e, []string{ws, conns, conn}, "schema:")
	sec := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema}, "sec:")
	wantTbl := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema, sec}, "tbl:")

	dl := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl) {
		var open bool
		sync(func() { open = m.modalOpen() })
		if !open {
			break
		}
		_ = tb.Inject(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
		time.Sleep(30 * time.Millisecond)
	}

	// CLICK the table's row — the real user action, and the one that produced the
	// bug. golib's focusFromPointer walks outward from the click target; the fix
	// is that it now passes the (focus-declining) tree and lands on the explorer.
	var clickY int
	for y, ln := range strings.Split(tb.String(), "\n") {
		if strings.Contains(ln, "data_schema") {
			clickY = y
			break
		}
	}
	if clickY == 0 {
		t.Fatalf("the table row is not on screen:\n%s", tb.String())
	}
	var rowsBefore int
	sync(func() {
		m.editor.SetValue("")
		rowsBefore = len(e.tree.VisibleRows())
	})

	_ = tb.Inject(tui.MouseEvent{Kind: tui.MousePress, Button: tui.MouseLeft, X: 4, Y: clickY})
	_ = tb.Inject(tui.MouseEvent{Kind: tui.MouseRelease, Button: tui.MouseLeft, X: 4, Y: clickY})
	time.Sleep(150 * time.Millisecond)

	var focusedExplorer bool
	var selected string
	sync(func() {
		focusedExplorer = m.ctx.FocusWithin(m.explorerBox)
		if n, ok := e.tree.Selected(); ok {
			selected = n.ID()
		}
	})
	t.Logf("after click at y=%d: focusWithinExplorer=%v selected=%q (want %q)", clickY, focusedExplorer, selected, wantTbl)
	if !focusedExplorer {
		t.Fatal("the click did not focus the explorer pane at all")
	}

	_ = tb.Inject(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
	time.Sleep(250 * time.Millisecond)

	var editor string
	var rowsAfter int
	sync(func() {
		editor = m.editor.Value()
		rowsAfter = len(e.tree.VisibleRows())
	})
	t.Logf("click then Enter: rows %d → %d, editor = %q", rowsBefore, rowsAfter, editor)

	if rowsAfter > rowsBefore {
		t.Errorf("the tree EXPANDED the table (rows %d → %d) instead of scaffolding: "+
			"the explorer's Enter intercept was bypassed because the click focused "+
			"the tree rather than the explorer", rowsBefore, rowsAfter)
	}
	if !strings.Contains(editor, "SELECT * FROM") {
		t.Errorf("click then Enter on a table did not scaffold; editor = %q", editor)
	}
}
