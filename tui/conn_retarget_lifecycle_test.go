package tui

// Item 5, blocker 1 (lector, b33c97b): the earlier tests installed a `tbl:` root
// directly with connNames empty, which is NOT a production-reachable state —
// applyWorkspaces fills connNames before it installs connection roots, and table
// nodes only ever descend from those roots.
//
// So this drives the REAL lifecycle instead: workspace load → connections →
// connection → schema → tables section → table, each level through the actual
// async load path, then activates the table. It answers the question the
// synthetic tests could not: in production, is the connection NAME available at
// the moment a table is activated?

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tui "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// childWithPrefix waits for a node with the given id prefix to become visible,
// expanding along the way. Levels load asynchronously, so ExpandPath is called
// repeatedly — its own contract is that an unloaded node stops the walk after
// requesting expansion.
func childWithPrefix(t *testing.T, sync func(func()), e *explorer, path []string, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var found string
		sync(func() {
			if len(path) > 0 {
				e.tree.ExpandPath(path...)
			}
			for _, n := range e.tree.VisibleRows() {
				if strings.HasPrefix(n.ID(), prefix) {
					found = n.ID()
					return
				}
			}
		})
		if found != "" {
			return found
		}
		time.Sleep(25 * time.Millisecond)
	}
	var dump []string
	sync(func() {
		for _, n := range e.tree.VisibleRows() {
			dump = append(dump, n.ID())
		}
	})
	t.Fatalf("no visible node with prefix %q after expanding %v; visible: %v", prefix, path, dump)
	return ""
}

func TestProductionLifecycle_TableActivationHasTheConnectionName(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "lifecycle-passphrase-1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	b := sess.Bind()

	connID, err := b.CreateConnection(ctx, "bravo", "sqlite", filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := b.Run(ctx, connID, `CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	wsID, err := b.CreateWorkspace(ctx, "main")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := b.AttachConnection(ctx, wsID, connID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	m, tb, sync := mountedWith(t, sess)
	e := m.explorer
	sync(func() { e.Reload() })

	// Walk the real tree, discovering ids rather than hardcoding the grammar.
	ws := childWithPrefix(t, sync, e, nil, "ws:")
	conns := childWithPrefix(t, sync, e, []string{ws}, "conns:")
	conn := childWithPrefix(t, sync, e, []string{ws, conns}, "conn:")
	schema := childWithPrefix(t, sync, e, []string{ws, conns, conn}, "schema:")
	sec := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema}, "sec:")
	tbl := childWithPrefix(t, sync, e, []string{ws, conns, conn, schema, sec}, "tbl:")

	// THE question blocker 1 asks: is the name cached at this moment?
	var cachedName, quoted string
	sync(func() {
		cachedName = e.ConnName(connID)
		quoted = e.quoted[tbl]
	})
	t.Logf("at activation time: connNames[%d]=%q quoted=%q", connID, cachedName, quoted)

	// Put the cursor on the table and activate it the way a user does.
	var conn0 int64
	sync(func() {
		for i, n := range e.tree.VisibleRows() {
			if n.ID() == tbl {
				e.tree.SetCursor(i)
			}
		}
		m.activeConn, m.activeConnNm = 0, ""
		m.refreshQueryTitle()
		conn0 = m.activeConn
		e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
	})
	if conn0 != 0 {
		t.Fatalf("precondition: activeConn = %d, want 0", conn0)
	}

	var gotConn int64
	var gotNm string
	sync(func() { gotConn, gotNm = m.activeConn, m.activeConnNm })
	if gotConn != connID {
		t.Fatalf("activeConn = %d, want %d — the real lifecycle did NOT retarget", gotConn, connID)
	}

	title := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ln := range strings.Split(tb.String(), "\n") {
			if strings.Contains(ln, "query ") {
				title = ln
			}
		}
		if strings.Contains(title, "query → ") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("activeConn=%d activeConnNm=%q title=%q", gotConn, gotNm, strings.TrimSpace(title))

	// If production really does have the name, the title shows it and the id
	// fallback is defensive only — which is exactly what lector asked me to settle.
	if gotNm == "" {
		t.Errorf("PRODUCTION-REACHABLE missing name: connNames[%d] was empty at "+
			"activation, so the reported symptom IS reachable and the fallback is "+
			"load-bearing rather than defensive", connID)
	}
	if !strings.Contains(title, "bravo") {
		t.Errorf("title %q does not name the connection; want it to contain \"bravo\"", strings.TrimSpace(title))
	}
	_ = widget.NewTreeNode // keep the import honest if the walk changes
}

// The asymmetry the synthetic tests hid: `case "conn"` sets BOTH activeWs and
// activeConn; `case "schema","sec","tbl","col","fn"` sets only activeConn. The
// `tbl:` id grammar is tbl:<connID>:<schema>:<name> — it carries no workspace —
// so noteConnFromNode structurally CANNOT update activeWs from it.
//
// activeWs is not cosmetic. It selects the workspace for note creation
// (ui.go:622), for saveNoteAs (ui.go:660), and for the connection picker
// (ui.go:886). A stale value therefore lists the wrong workspace's connections
// in SPC C — which looks exactly like "the connection did not change" — and
// files a saved note into the wrong workspace.
func TestProductionLifecycle_TableActivationLeavesTheWorkspaceStale(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "lifecycle-passphrase-2"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	b := sess.Bind()

	mk := func(name, file, ws string) (int64, int64) {
		cid, err := b.CreateConnection(ctx, name, "sqlite", filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("conn %s: %v", name, err)
		}
		if _, err := b.Run(ctx, cid, `CREATE TABLE t (v TEXT)`); err != nil {
			t.Fatalf("ddl %s: %v", name, err)
		}
		wid, err := b.CreateWorkspace(ctx, ws)
		if err != nil {
			t.Fatalf("ws %s: %v", ws, err)
		}
		if err := b.AttachConnection(ctx, wid, cid); err != nil {
			t.Fatalf("attach %s: %v", ws, err)
		}
		return wid, cid
	}
	ws1, _ := mk("alpha", "a.db", "first")
	ws2, conn2 := mk("bravo", "b.db", "second")

	m, _, sync := mountedWith(t, sess)
	e := m.explorer
	sync(func() { e.Reload() })

	// Land on workspace ONE via its connection node — the path that works.
	sync(func() { m.noteConnFromNode("conn:" + strconv.FormatInt(ws1, 10) + ":1") })
	var gotWs int64
	sync(func() { gotWs = m.activeWs })
	if gotWs != ws1 {
		t.Fatalf("precondition: activeWs = %d, want %d", gotWs, ws1)
	}

	// Now activate a TABLE that lives under workspace TWO's connection.
	wsN := "ws:" + strconv.FormatInt(ws2, 10)
	conns := childWithPrefix(t, sync, e, []string{wsN}, "conns:")
	connNode := childWithPrefix(t, sync, e, []string{wsN, conns}, "conn:"+strconv.FormatInt(ws2, 10)+":")
	schema := childWithPrefix(t, sync, e, []string{wsN, conns, connNode}, "schema:"+strconv.FormatInt(conn2, 10)+":")
	sec := childWithPrefix(t, sync, e, []string{wsN, conns, connNode, schema}, "sec:")
	tbl := childWithPrefix(t, sync, e, []string{wsN, conns, connNode, schema, sec}, "tbl:")

	var gotConn int64
	sync(func() {
		for i, n := range e.tree.VisibleRows() {
			if n.ID() == tbl {
				e.tree.SetCursor(i)
			}
		}
		e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
		gotConn, gotWs = m.activeConn, m.activeWs
	})

	t.Logf("after activating a table under ws%d: activeConn=%d (want %d) activeWs=%d (want %d)",
		ws2, gotConn, conn2, gotWs, ws2)

	if gotConn != conn2 {
		t.Errorf("activeConn = %d, want %d", gotConn, conn2)
	}
	if gotWs != ws2 {
		t.Errorf("activeWs = %d but the activated table lives in workspace %d — "+
			"a note saved now goes to the WRONG workspace (saveNoteAs uses activeWs, "+
			"ui.go:660) and SPC C lists the wrong workspace's connections (ui.go:886)",
			gotWs, ws2)
	}
}
