package tui

// Item 2 consumer side: a DOUBLE-CLICK on a table must do what ENTER does —
// scaffold a query — with no autodb code change, because the explorer already
// subscribes to widget.ActivateEvent and golib now publishes it on a
// double-click for branches as well as leaves (golib ADR-0010 §2.5).
//
// This is the assertion that makes "no autodb change is needed" a measured
// claim rather than a reading of the subscription.

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

func TestDoubleClickOnATableScaffolds(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "dblclick-passphrase-1"); err != nil {
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
	childWithPrefix(t, sync, e, []string{ws, conns, conn, schema, sec}, "tbl:")

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

	clickY := 0
	for y, ln := range strings.Split(tb.String(), "\n") {
		if strings.Contains(ln, "data_schema") {
			clickY = y
			break
		}
	}
	if clickY == 0 {
		t.Fatalf("table row not on screen:\n%s", tb.String())
	}

	var rowsBefore int
	sync(func() {
		m.editor.SetValue("")
		rowsBefore = len(e.tree.VisibleRows())
	})

	// TWO presses on the same cell: a real double-click. The count is synthesised
	// by the App, so a test cannot forge it.
	for i := 0; i < 2; i++ {
		_ = tb.Inject(tui.MouseEvent{Kind: tui.MousePress, Button: tui.MouseLeft, X: 4, Y: clickY})
		_ = tb.Inject(tui.MouseEvent{Kind: tui.MouseRelease, Button: tui.MouseLeft, X: 4, Y: clickY})
	}
	time.Sleep(300 * time.Millisecond)

	var editor string
	var rowsAfter int
	sync(func() {
		editor = m.editor.Value()
		rowsAfter = len(e.tree.VisibleRows())
	})
	t.Logf("double-click: rows %d → %d, editor = %q", rowsBefore, rowsAfter, editor)

	if !strings.Contains(editor, "SELECT * FROM") {
		t.Errorf("a double-click on a table did not scaffold; editor = %q", editor)
	}
	if rowsAfter > rowsBefore {
		t.Errorf("the tree also EXPANDED the table (rows %d → %d): activation and "+
			"expansion must not both happen", rowsBefore, rowsAfter)
	}
}
