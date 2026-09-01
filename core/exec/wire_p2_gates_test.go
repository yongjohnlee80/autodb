package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// PR #41 r0, lector's P2 finding: WireExecute omitted the statement-size
// invariant the token path enforces, so the two surfaces had drifted — a
// statement of any size reached the classifier here while the identical
// statement was refused on the token path.

// --------------------------------------------------------------------------
// P2 — WireExecute must reject oversized input BEFORE classification.
// --------------------------------------------------------------------------

// TestWireExecute_RejectsOversizedBeforeClassification pins the size invariant
// on the wire path.
//
// The boundary case is the half that matters. Asserting only that a huge
// statement is refused would pass against a gate placed anywhere at all,
// including one that refuses everything; asserting that cap-sized input gets
// PAST the size gate is what makes the pair discriminating.
func TestWireExecute_RejectsOversizedBeforeClassification(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)
	_ = connID

	cap := f.eng.maxStatementBytes

	// OVER the bound: refused as too large, and refused for THAT reason.
	over := "SELECT '" + strings.Repeat("x", cap) + "'"
	_, err := f.eng.WireExecute(ctx, sid, userID, over, testIP)
	if !errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("a %d-byte statement returned %v, want ErrScriptTooLarge — "+
			"the wire path classified and routed input the token path refuses", len(over), err)
	}

	// AT the bound: must not be refused for SIZE. It may fail for any other
	// reason (syntax, gate, target); what it must not be is too large.
	atCap := "SELECT '" + strings.Repeat("y", cap-len("SELECT ''")) + "'"
	if len(atCap) != cap {
		t.Fatalf("test constructed a %d-byte statement, want exactly %d", len(atCap), cap)
	}
	if _, err := f.eng.WireExecute(ctx, sid, userID, atCap, testIP); errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("a statement of exactly %d bytes was refused as too large; the bound is "+
			"inclusive on the token path and must be here too", cap)
	}
}

// TestWireExecute_OversizedControlIsRefusedToo covers the half lector named
// explicitly: the gate sits above Classify, so CONTROL statements are governed
// by it as well. A gate placed after classification, or inside the non-control
// branch, would let this through.
func TestWireExecute_OversizedControlIsRefusedToo(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)

	// A control verb padded past the cap by a comment. It classifies as
	// control, so it takes the wireControl route.
	over := "COMMIT -- " + strings.Repeat("z", f.eng.maxStatementBytes)
	if _, err := f.eng.WireExecute(ctx, sid, userID, over, testIP); !errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("an oversized CONTROL statement returned %v, want ErrScriptTooLarge — "+
			"the size gate is below the control routing rather than above it", err)
	}
}
