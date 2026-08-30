package tui

// Execution-level coverage for item 5: activating a table must retarget the
// connection the query actually RUNS on, not merely what the title says.
//
// Two connections hold a table with the SAME name and DIFFERENT contents,
// which is the dangerous case Johno described: running the scaffold against
// the wrong database succeeds silently instead of erroring, so only the
// returned rows can tell the two apart. A title-only assertion would pass
// while the wrong database is queried.

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
	tui "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

func bootServer(t *testing.T) string {
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
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "item5", rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

func TestEnterOnTable_QueryExecutesAgainstThatTablesConnection(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "item5-passphrase-长"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	b := sess.Bind()

	// Same table name, different contents — only the rows distinguish them.
	mk := func(name, file, val string) int64 {
		id, err := b.CreateConnection(ctx, name, "sqlite", filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		for _, stmt := range []string{
			`CREATE TABLE t (v TEXT)`,
			`INSERT INTO t (v) VALUES ('` + val + `')`,
		} {
			if _, err := b.Run(ctx, id, stmt); err != nil {
				t.Fatalf("%s: %q: %v", name, stmt, err)
			}
		}
		return id
	}
	connA := mk("alpha", "a.db", "row-from-ALPHA")
	connB := mk("bravo", "b.db", "row-from-BRAVO")

	m, _, sync := mountedWith(t, sess)

	// Start on A through the real connection-node path, exactly as pressing
	// Enter on a connection does.
	sync(func() { m.noteConnFromNode("conn:1:" + strconv.FormatInt(connA, 10)) })
	var got int64
	sync(func() { got = m.activeConn })
	if got != connA {
		t.Fatalf("precondition: activeConn = %d, want alpha (%d)", got, connA)
	}

	// Now activate a TABLE that lives under B.
	tbl := "tbl:" + strconv.FormatInt(connB, 10) + ":" + encSeg("main") + ":" + encSeg("t")
	sync(func() {
		e := m.explorer
		e.tree.SetRoots(widget.NewTreeNode(tbl, "t", widget.WithLeaf()))
		e.tree.SetCursor(0)
		e.quoted[tbl] = "t"
		e.HandleEvent(tui.KeyEvent{Kind: tui.KeyPress, Code: tui.KeyEnter})
	})
	sync(func() { got = m.activeConn })
	if got != connB {
		t.Fatalf("activeConn = %d after Enter on a table under bravo (%d) — not retargeted", got, connB)
	}

	// Run the scaffold the activation loaded, and read the ROWS. Asserted on the
	// results panel rather than the rendered screen: the About splash sits over
	// the grid on a fresh model, and the rows are the actual evidence of which
	// database answered.
	sync(func() { m.runQuery() })

	var cells string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		sync(func() {
			if m.results != nil && m.results.res != nil {
				var sb strings.Builder
				for _, row := range m.results.res.Rows {
					for _, v := range row {
						sb.WriteString(renderCell(v))
						sb.WriteString(" ")
					}
				}
				cells = sb.String()
			}
		})
		if strings.Contains(cells, "row-from-") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	switch {
	case strings.Contains(cells, "row-from-BRAVO"):
		t.Logf("correct: query ran on bravo (conn %d); rows = %q", connB, cells)
	case strings.Contains(cells, "row-from-ALPHA"):
		t.Fatalf("the query executed against ALPHA — the WRONG database — after "+
			"activating a table under BRAVO. Both have table t, so it succeeded "+
			"with the wrong rows: %q", cells)
	default:
		// Distinguish the three nil-result shapes instead of conflating them:
		// a query error lands in statusMsg (execDone error branch), a
		// superseded result clears running and announces itself in the
		// status ("query superseded by a reconnect"), and a genuine
		// timeout leaves running=true.
		var status string
		var running bool
		var gen uint64
		sync(func() { status, running, gen = m.statusMsg, m.running, m.session.Gen() })
		t.Fatalf("no rows returned; cannot tell which connection ran: %q\n"+
			"  status=%q running=%v session.gen=%d\n"+
			"  (error → status carries it; superseded → running=false, status "+
			"says superseded; timeout → running=true)", cells, status, running, gen)
	}
}

// Call site 3 of 3: the "running on …" status. Read inside the SAME App.Update
// as runSQL, because setStatus happens before the query goroutine is launched —
// so this is deterministic rather than a race against the result arriving.
func TestRunStatus_UsesTheConnectionLabel(t *testing.T) {
	addr := bootServer(t)
	dir := t.TempDir()

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "runstatus-passphrase-1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cid, err := sess.Bind().CreateConnection(ctx, "bravo", "sqlite", filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	m, _, sync := mountedWith(t, sess)
	var status string
	sync(func() {
		m.activeConn, m.activeConnNm = cid, "" // real connection, name not cached
		m.runSQL("SELECT 1")
		status = m.statusMsg
	})
	want := "running on connection " + strconv.FormatInt(cid, 10)
	if !strings.Contains(status, want) {
		t.Errorf("status = %q, want it to contain %q — an empty label renders "+
			"\"running on …\", which names nothing", status, want)
	}
}
