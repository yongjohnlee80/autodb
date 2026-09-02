package exec

import (
	"context"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// ADR-0075 F3a item 2 — "expose the debug profile through the front door". The
// mechanism already exists on the connection (meta.Connection.Debug →
// txLimits.forConnection at BEGIN); this cell proves a WIRE session's
// transaction takes its idle-in-transaction bound from it exactly as the token
// path does: on a debug-flagged connection the sweep leaves a transaction idle
// for two minutes alone (the bound is DefaultDebugIdleInTxTimeout, 10 min); on
// a normal connection the same sweep rolls it back (90 s). The sweep runs with
// a synthetic clock, so no wall time is spent.
func TestWireDebugProfile_ConnectionFlagGovernsTheWireSessionsIdleBound(t *testing.T) {
	ctx := context.Background()
	// A normal connection: the fixture opens a real transaction (status T).
	fn, connN, sidN, _, userN := pgWireSession(t)
	if st, _ := fn.eng.WireTxStatus(sidN, userN); st != TxStatusInTx {
		t.Fatalf("normal fixture status %q, want T", st)
	}
	// A debug-flagged connection, its own fixture and engine.
	fd, connD, sidD, _, userD := pgWireSession(t)
	if _, err := fd.eng.WireExecute(ctx, sidD, userD, "ROLLBACK", testIP); err != nil {
		t.Fatalf("ROLLBACK before flagging: %v", err)
	}
	if err := fd.store.Connections.OnCtx(ctx).With(meta.ConnID, connD).Set(meta.ConnDebug, 1).Update(); err != nil {
		t.Fatalf("flag connection %d debug: %v", connD, err)
	}
	// Limits are resolved at BEGIN from the connection: open the transaction AFTER flagging.
	if _, err := fd.eng.WireExecute(ctx, sidD, userD, "BEGIN", testIP); err != nil {
		t.Fatalf("BEGIN on the debug connection: %v", err)
	}
	if st, _ := fd.eng.WireTxStatus(sidD, userD); st != TxStatusInTx {
		t.Fatalf("debug fixture status %q, want T", st)
	}

	// Two minutes idle: past the 90 s default, inside the 10 min debug bound.
	later := time.Now().Add(2 * time.Minute)
	if n := fn.eng.reapExpired(ctx, later); n != 1 {
		t.Fatalf("normal connection: the sweep acted on %d session(s) at 2 min idle, want 1 — the 90 s bound must roll the transaction back", n)
	}
	if st, err := fn.eng.WireTxStatus(sidN, userN); err == nil && st == TxStatusInTx {
		t.Fatalf("normal connection: transaction still open after the sweep (status %q)", st)
	}
	if n := fd.eng.reapExpired(ctx, later); n != 0 {
		t.Fatalf("debug connection: the sweep acted on %d session(s) at 2 min idle, want 0 — the wire session must take the connection's debug bound", n)
	}
	if st, err := fd.eng.WireTxStatus(sidD, userD); err != nil || st != TxStatusInTx {
		t.Fatalf("debug connection: status %q err %v after the sweep, want T (still open)", st, err)
	}
	// Past the debug bound the transaction is rolled back too — the bound is a bound.
	if n := fd.eng.reapExpired(ctx, time.Now().Add(DefaultDebugIdleInTxTimeout+time.Minute)); n != 1 {
		t.Fatalf("debug connection: the sweep acted on %d session(s) past the 10 min debug bound, want 1", n)
	}
	_ = connN
}
