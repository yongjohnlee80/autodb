package exec

// AN EMPTY STATEMENT IS LEGAL ON THE EXTENDED PROTOCOL.
//
// pgjdbc validates a connection by parsing an EMPTY statement, on EVERY
// connection, before it runs anything. PostgreSQL accepts that: Parse("") is a
// legal prepared statement that yields EmptyQueryResponse instead of
// CommandComplete. autodb refused it with gate/refused, so every JetBrains data
// source died on its own probe and reported the useless "connection was
// established but closed as invalid" (Johno, 2026-09-03 manual testing).
//
// The SIMPLE path already had this right, and the protocol matrix row for
// `Query` states it outright — "Empty query -> EmptyQueryResponse +
// ReadyForQuery". The extended path was complete for the vocabulary its cells
// exercised, and every one of them drove a non-empty statement. That is the
// same gap twice in one week; see the enumerate-from-the-spec convention.

import (
	"context"
	"fmt"
	"testing"
)

// extSegment drives one statement through a WHOLE segment — Parse, Bind,
// Execute, Sync — and returns every frame the client sees.
//
// The Sync is load-bearing and its absence is what the first draft of this cell
// got wrong: extended-protocol replies are QUEUED and delivered at the segment
// boundary, so a Parse/Bind/Execute with no Sync legitimately emits nothing. A
// cell that stopped at Execute reported the fix broken when the fix was fine —
// and, worse, the same cell would have passed a mutation that queued nothing at
// all. Every real client ends its segment with Sync; so does this.
func extSegment(t *testing.T, f *fixture, sid SessionID, userID int64, name, sql string) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	if err := f.eng.WireParse(ctx, sid, userID, name, sql, nil, testIP); err != nil {
		return nil, err
	}
	if err := f.eng.WireBind(ctx, sid, userID, name, name, nil, nil, nil); err != nil {
		return nil, err
	}
	if err := f.eng.WireExecutePortal(ctx, sid, userID, name, 0, testIP, func(WireMessage) error {
		return nil
	}); err != nil {
		return nil, err
	}
	var kinds []string
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		kinds = append(kinds, m.Kind)
		return nil
	}); err != nil {
		return nil, err
	}
	return kinds, nil
}

// TestExtended_EmptyStatementYieldsEmptyQueryResponse drives a whole segment
// exactly as a client does and asserts the frame PostgreSQL would send.
func TestExtended_EmptyStatementYieldsEmptyQueryResponse(t *testing.T) {
	f, _, sid, userID := extSession(t)

	kinds, err := extSegment(t, f, sid, userID, "probe", "")
	if err != nil {
		t.Fatalf("an empty statement was refused: %v\n"+
			"pgjdbc sends exactly this on every connection, so a refusal here "+
			"makes the front door unusable from every JetBrains data source", err)
	}
	if !contains(kinds, "EmptyQueryResponse") {
		t.Fatalf("the segment produced %v, with no EmptyQueryResponse", kinds)
	}
	// NOT CommandComplete. That substitution is the likely wrong fix — it would
	// satisfy a cell that only checked "some completion frame arrived", and a
	// driver reading a command tag for a statement that ran nothing is exactly
	// the confusion this frame exists to prevent.
	if contains(kinds, "CommandComplete") {
		t.Fatalf("got CommandComplete for an empty statement; PostgreSQL sends "+
			"EmptyQueryResponse and NO command tag. frames: %v", kinds)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestExtended_EmptyStatementIsAWholeObject: Parse("") creates a real protocol
// object, so a client may Describe and Close it like any other. pgjdbc does
// exactly that, and a fix that only taught Execute about empty would leave the
// probe failing one frame later.
func TestExtended_EmptyStatementIsAWholeObject(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	if err := f.eng.WireParse(ctx, sid, userID, "obj", "", nil, testIP); err != nil {
		t.Fatalf("parse of an empty statement: %v", err)
	}
	if err := f.eng.WireDescribeStatement(ctx, sid, userID, "obj"); err != nil {
		t.Fatalf("describe of an empty statement: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "obj", "obj", nil, nil, nil); err != nil {
		t.Fatalf("bind of an empty statement: %v", err)
	}
	if err := f.eng.WireDescribePortal(ctx, sid, userID, "obj"); err != nil {
		t.Fatalf("describe of an empty portal: %v", err)
	}
	if err := f.eng.WireClosePortal(ctx, sid, userID, "obj"); err != nil {
		t.Fatalf("close of an empty portal: %v", err)
	}
	if err := f.eng.WireCloseStatement(ctx, sid, userID, "obj"); err != nil {
		t.Fatalf("close of an empty statement: %v", err)
	}
}

// TestExtended_CommentOnlyStatementIsEmpty: a buffer of only comments carries no
// statement either, and Classify reports it the same way. A driver that sends a
// commented-out query must get the empty answer, not a refusal.
func TestExtended_CommentOnlyStatementIsEmpty(t *testing.T) {
	f, _, sid, userID := extSession(t)

	for i, sql := range []string{"-- nothing here", "/* nor here */", "  \n\t "} {
		kinds, err := extSegment(t, f, sid, userID, fmt.Sprintf("c%d", i), sql)
		if err != nil {
			t.Fatalf("%q was refused: %v", sql, err)
		}
		if !contains(kinds, "EmptyQueryResponse") {
			t.Fatalf("%q produced %v, with no EmptyQueryResponse", sql, kinds)
		}
		if contains(kinds, "CommandComplete") {
			t.Fatalf("%q produced a command tag: %v", sql, kinds)
		}
	}
}
