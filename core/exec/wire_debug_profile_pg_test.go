package exec

import (
	"context"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// "Expose the debug profile through the front door". The
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

	// THE FLAG NO LONGER SELECTS ANYTHING, and this cell now proves that
	// rather than the opposite.
	//
	// It used to assert that a debug-flagged connection got a LONGER bound: at
	// two minutes idle the ordinary session was reaped by the ninety-second
	// limit and the debug one survived to its ten-minute one. Both halves of
	// that are gone. Every session is now treated as a debugging session and
	// both take the same two-hour bound, so a cell asserting they differ is
	// asserting behaviour that was deliberately retired.
	//
	// Retained rather than deleted, inverted rather than loosened: "the flag
	// changes nothing" is a claim worth a cell, and it is the claim a reader
	// of the deprecated flag most needs answered.
	inside := time.Now().Add(defaultTxLimits().idleInTx - time.Minute)
	if n := fn.eng.reapExpired(ctx, inside); n != 0 {
		t.Fatalf("normal connection: the sweep acted on %d session(s) inside the bound, want 0", n)
	}
	if n := fd.eng.reapExpired(ctx, inside); n != 0 {
		t.Fatalf("debug connection: the sweep acted on %d session(s) inside the bound, want 0", n)
	}
	for _, c := range []struct {
		name   string
		status func() (byte, error)
	}{
		{"normal", func() (byte, error) { return fn.eng.WireTxStatus(sidN, userN) }},
		{"debug", func() (byte, error) { return fd.eng.WireTxStatus(sidD, userD) }},
	} {
		if st, err := c.status(); err != nil || st != TxStatusInTx {
			t.Fatalf("%s connection: status %q err %v inside the bound, want T (still open)",
				c.name, st, err)
		}
	}

	// And past it, BOTH are reaped, at the same instant.
	past := time.Now().Add(defaultTxLimits().idleInTx + time.Minute)
	if n := fn.eng.reapExpired(ctx, past); n != 1 {
		t.Fatalf("normal connection: the sweep acted on %d session(s) past the bound, want 1", n)
	}
	if n := fd.eng.reapExpired(ctx, past); n != 1 {
		t.Fatalf("debug connection: the sweep acted on %d session(s) past the bound, want 1 — "+
			"the deprecated flag must not buy a longer bound", n)
	}

	// THE TWO BOUNDS ARE ONE NUMBER. Configuration refuses a deprecated value
	// that differs from the common one, so a cell that let them drift apart
	// would describe a daemon that cannot start.
	if DefaultDebugIdleInTxTimeout != DefaultIdleInTxTimeout {
		t.Errorf("the debug bound (%v) differs from the common one (%v); the flag selects "+
			"something again", DefaultDebugIdleInTxTimeout, DefaultIdleInTxTimeout)
	}
	_ = connN
}
