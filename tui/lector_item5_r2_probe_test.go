package tui

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/yongjohnlee80/golib/logger"
	tui "github.com/yongjohnlee80/golib/tui"
)

// A connection may be attached to multiple workspaces. A connID -> wsID cache
// can represent only one of those rendered parents, so one table subtree must
// resolve to the wrong workspace.
//
// Written by lector reviewing item 5 r2; it failed and the cache was replaced by
// rendered-ancestry resolution (explorer.WorkspaceOfNode). ADAPTED, not weakened:
// the original used e.ConnWorkspace to pick which of the two workspaces the cache
// would get wrong, and that method is gone because the cache WAS the defect. With
// ancestry there is no privileged one, so both subtrees are now checked — a
// strictly stronger assertion that does not name the mechanism.
func TestLectorProbe_SharedConnectionKeepsItsRenderedWorkspace(t *testing.T) {
	addr := bootServer(t)
	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "lector-item5-r2-probe"); err != nil {
		t.Fatal(err)
	}
	b := sess.Bind()
	cid, err := b.CreateConnection(ctx, "shared", "sqlite", filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, cid, `CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	ws1, err := b.CreateWorkspace(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := b.CreateWorkspace(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []int64{ws1, ws2} {
		if err := b.AttachConnection(ctx, ws, cid); err != nil {
			t.Fatal(err)
		}
	}

	m, _, sync := mountedWith(t, sess)
	e := m.explorer
	sync(func() { e.Reload() })
	childWithPrefix(t, sync, e, nil, "ws:"+strconv.FormatInt(ws1, 10))
	childWithPrefix(t, sync, e, nil, "ws:"+strconv.FormatInt(ws2, 10))

	// Both rendered subtrees of the SAME connection must resolve to their own
	// workspace. A cache can satisfy at most one of these two iterations.
	for _, target := range []int64{ws1, ws2} {
		wsNode := "ws:" + strconv.FormatInt(target, 10)
		conns := childWithPrefix(t, sync, e, []string{wsNode}, "conns:"+strconv.FormatInt(target, 10))
		conn := childWithPrefix(t, sync, e, []string{wsNode, conns}, "conn:"+strconv.FormatInt(target, 10)+":"+strconv.FormatInt(cid, 10))
		schema := childWithPrefix(t, sync, e, []string{wsNode, conns, conn}, "schema:"+strconv.FormatInt(cid, 10)+":")
		sec := childWithPrefix(t, sync, e, []string{wsNode, conns, conn, schema}, "sec:")
		tbl := childWithPrefix(t, sync, e, []string{wsNode, conns, conn, schema, sec}, "tbl:")

		var got int64
		sync(func() {
			m.activeWs = 0
			for i, n := range e.tree.VisibleRows() {
				if n.ID() == tbl {
					e.tree.SetCursor(i)
				}
			}
			e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
			got = m.activeWs
		})
		if got != target {
			t.Errorf("the table rendered under workspace %d activated workspace %d "+
				"(connection %d is attached to both %d and %d, so only its rendered "+
				"position can tell them apart)", target, got, cid, ws1, ws2)
		}
	}
}
