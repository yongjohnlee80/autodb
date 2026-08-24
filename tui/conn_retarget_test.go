package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tui "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// DIAGNOSTIC (item 5): does the connection retarget when the explorer cursor
// lands on a table? Johno reports the query connection changes only when Enter
// is pressed on the CONNECTION node, never on a table under it — which would
// mean a scaffold naming one database can execute against another.
//
// This bisects the question: the state assignment itself, before any UI path.
func TestNoteConnFromNode_TableRetargetsTheConnection(t *testing.T) {
	m := unconnected()
	m.activeConn, m.activeConnNm = 1, "first"

	// The real grammar: tbl:<connID>:<schema>:<name> (explorer.go:316).
	id := "tbl:2:" + encSeg("public") + ":" + encSeg("data_schema")
	m.noteConnFromNode(id)

	if m.activeConn != 2 {
		t.Fatalf("activeConn = %d, want 2 — a table under connection 2 did not retarget", m.activeConn)
	}
	t.Logf("activeConn=%d activeConnNm=%q", m.activeConn, m.activeConnNm)
}

// The connection node path, which Johno reports DOES work — a control that tells
// us whether the two differ at this level or higher up.
func TestNoteConnFromNode_ConnectionNodeRetargets(t *testing.T) {
	m := unconnected()
	m.activeConn, m.activeConnNm = 1, "first"
	m.noteConnFromNode("conn:7:2") // conn:<ws>:<connID>
	if m.activeConn != 2 {
		t.Fatalf("activeConn = %d, want 2", m.activeConn)
	}
	if m.activeWs != 7 {
		t.Fatalf("activeWs = %d, want 7", m.activeWs)
	}
}

// mounted builds a Model inside a running headless App, so ctx-dependent paths
// (focus, MarkDirty, titles) work. Mutations and reads go through App.Update,
// which is the event loop's own goroutine — reading widget state directly from
// the test goroutine is a data race that -race catches.
func mounted(t *testing.T) (*Model, *tui.TestBackend, func(func())) {
	return mountedWith(t, nil)
}

// mountedWith mounts a Model on a caller-supplied session, or an unconnected one.
func mountedWith(t *testing.T, sess *Session) (*Model, *tui.TestBackend, func(func())) {
	t.Helper()
	m := unconnected()
	if sess != nil {
		m = New(sess, nil, nil)
	}
	tb := tui.NewTestBackend(110, 32)
	app := tui.NewApp(m.Root(), tui.WithBackend(tb), tui.WithMinFrameInterval(0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	sync := func(fn func()) {
		ch := make(chan struct{})
		app.Update(func() { fn(); close(ch) })
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("App.Update did not run: event loop stalled")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ready bool
		sync(func() { ready = m.explorer != nil && m.explorer.ctx != nil })
		if ready {
			return m, tb, sync
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("model never mounted")
	return nil, nil, nil
}

// Caller-level: drive the explorer's REAL Enter path. The state assignment is
// already proven correct above, so anything wrong is here or in what the user
// can SEE afterwards — which is why this asserts the query title too.
func TestExplorerEnterOnTable_RetargetsTheConnection(t *testing.T) {
	m, tb, sync := mounted(t)

	tbl := "tbl:2:" + encSeg("public") + ":" + encSeg("data_schema")
	var conn int64
	var nm string
	sync(func() {
		m.activeConn, m.activeConnNm = 1, "first"
		e := m.explorer
		e.tree.SetRoots(widget.NewTreeNode(tbl, "data_schema", widget.WithLeaf()))
		e.tree.SetCursor(0)
		e.quoted[tbl] = `"public"."data_schema"`
		e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
		conn, nm = m.activeConn, m.activeConnNm
	})

	if conn != 2 {
		t.Errorf("activeConn = %d, want 2 — Enter on a table did not retarget", conn)
	}
	// The screen is what the user actually reads.
	screen := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		screen = tb.String()
		// Poll for the RETARGETED title, not merely for the word "query" — the
		// pre-fix title also contains it, so breaking early reads a stale frame.
		if strings.Contains(screen, "query \u2192 ") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	titleLine := ""
	for _, ln := range strings.Split(screen, "\n") {
		if strings.Contains(ln, "query") {
			titleLine = strings.TrimSpace(ln)
			break
		}
	}
	t.Logf("activeConn=%d activeConnNm=%q\n  query title line: %s", conn, nm, titleLine)

	// What the user actually reads. A retarget the UI reports as "no connection"
	// is indistinguishable from no retarget at all, which is the reported symptom.
	if conn == 2 && strings.Contains(titleLine, "no connection") {
		t.Errorf("connection DID retarget to %d but the UI reports %q — "+
			"the state is right and the display is wrong, which is "+
			"indistinguishable from no retarget at all", conn, titleLine)
	}
}
