package tui

// THE CLIENT DECODER ITSELF, WHICH NOTHING CROSSED.
//
// Review found this after the first fold, by removing one line:
//
//	Suspended: mB(m, "suspended")
//
// and watching `go test ./core/exec ./rpc ./tui -count=1` stay GREEN. I
// reproduced it. In that mutant the daemon emits the correct key, the renderer
// marks a page correctly when handed Suspended: true, and the real product
// always decodes false — so an operator is told a suspended page completed,
// which is the whole defect this PR exists to remove.
//
// The gap was in the shape of my evidence, not in any one cell. The RPC cell
// stops at the raw response map; the render cells construct HistoryRow
// literals. Both sides of the boundary were tested and the boundary was not.
//
// So this drives the PRODUCTION path: a real RPC server, a real Session, a
// real Bound.History, and rows seeded into the store so what comes back has
// been through msgpack and the decoder rather than through a struct literal.

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
)

// bootServerWithStore is bootServer plus the store, so a cell can put history
// rows in front of the real decoder.
//
// Seeded rather than executed: nothing autodb ships sends a row limit, so no
// in-process driver can produce a suspended Execute. The live suspension is
// covered against real PostgreSQL in core/exec; what is under test here is the
// decode.
func bootServerWithStore(t *testing.T) (string, *meta.Store) {
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
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "history-boundary",
		rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), store
}

func TestBoundHistory_DecodesTheSuspensionAxis(t *testing.T) {
	addr, store := bootServerWithStore(t)

	sess := NewSession(addr, logger.Nop{}, nil)
	t.Cleanup(sess.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := sess.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sess.Bind().Bootstrap(ctx, "root", "history-boundary-passphrase"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	b := sess.Bind()

	// A real connection, because script_history carries a foreign key to it —
	// a hand-picked id fails the constraint, which is the store keeping the
	// audit trail referential rather than an inconvenience to route around.
	connID, err := b.CreateConnection(ctx, "boundary", "sqlite",
		filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	// Two rows differing ONLY in the flag, with the SAME status. The pairing
	// is the measurement: a decoder that hard-coded either value, or derived
	// it from status, fails one of the two.
	const suspendedScript = "SELECT 'a-suspended-page'"
	const completedScript = "SELECT 'a-completed-statement'"
	seed := func(script string, suspended int64) {
		t.Helper()
		if _, err := store.History.OnCtx(ctx).
			Set(meta.HistUserID, int64(1)).Set(meta.HistConnID, connID).
			Set(meta.HistIP, "127.0.0.1").Set(meta.HistScript, script).
			Set(meta.HistStartedAt, int64(1788900000)).
			Set(meta.HistDurationMS, int64(7)).Set(meta.HistRowCount, int64(3)).
			Set(meta.HistStatus, "ok").Set(meta.HistError, "").
			Set(meta.HistTxID, "").Set(meta.HistSuspended, suspended).
			Insert(); err != nil {
			t.Fatalf("seeding %q: %v", script, err)
		}
	}
	seed(suspendedScript, 1)
	seed(completedScript, 0)

	rows, err := b.History(ctx, 50)
	if err != nil {
		t.Fatalf("Bound.History: %v", err)
	}

	got := map[string]HistoryRow{}
	for _, r := range rows {
		switch r.Script {
		case suspendedScript, completedScript:
			got[r.Script] = r
		}
	}
	// PREMISE: both rows came back at all. Without this the assertions below
	// would be vacuous for a History that returned nothing.
	if len(got) != 2 {
		t.Fatalf("Bound.History returned %d of the 2 seeded rows (%d rows total) — the "+
			"cell cannot measure a decode it never received", len(got), len(rows))
	}

	// THE FINDING: the flag survives the wire and the decoder.
	if !got[suspendedScript].Suspended {
		t.Errorf("Bound.History decoded Suspended=false for a row stored as suspended. " +
			"The daemon emits the key and the renderer honours it, so with this decode " +
			"missing the real product silently tells an operator that a truncated page " +
			"completed — the defect this axis exists to remove")
	}
	// And the control: not decoded as true for everything.
	if got[completedScript].Suspended {
		t.Errorf("Bound.History decoded Suspended=true for a completed statement, so the " +
			"decode distinguishes nothing")
	}
	// The durability token still crosses unchanged, on both rows.
	for script, row := range got {
		if row.Status != "ok" {
			t.Errorf("%s decoded status %q, want \"ok\"", script, row.Status)
		}
	}

	// AND THE RENDER AGREES, so the boundary is joined to the surface an
	// operator reads rather than only to the struct.
	if want, mark := "3+", rowCountText(got[suspendedScript]); mark != want {
		t.Errorf("the suspended row renders its count as %q, want %q", mark, want)
	}
	if want, mark := "3", rowCountText(got[completedScript]); mark != want {
		t.Errorf("the completed row renders its count as %q, want %q", mark, want)
	}
}
