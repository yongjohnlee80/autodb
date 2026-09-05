package frontdoor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// The EXECUTE row's authority half — cited deliberately WITHOUT the row-key
// form, because a citing cell claims to witness the whole row and this one
// witnesses one of its two contracted halves. Authority is re-resolved at every
// Execute, "portal
// re-executions included".
//
// THE DISCRIMINATING SHAPE, named by white-vision and unreachable until now.
// The existing live proof revokes a grant between PARSE and EXECUTE, which is
// not a re-execution: a resumption riding the FIRST Execute's authority would
// pass it, and would pass every other cell written today. The only shape that
// separates the two is to revoke BETWEEN THE FIRST EXECUTE AND THE RESUMPTION.
//
// Why it matters concretely: a paged read is a portal a client comes back to,
// and the gap between pages is operator time — long enough for an admin to
// revoke a grant precisely because they want the reading to stop. If authority
// were resolved once at the first Execute, the revocation would not take
// effect until the client had finished paging through everything it asked for,
// which is the opposite of what revoking a grant means.
// HOW THIS IS EVIDENCED, stated because the usual mutation is not what proves
// it. Two measurements together:
//
//  1. THE REFUSAL ARRIVES ON THE EXECUTE, not on the Sync. Driven separately
//     as Execute + Flush with NO Sync: the 42501 comes back on the Flush. That
//     matters because a refusal produced by Sync re-authorizing the whole
//     segment would be a different claim than the row makes, and this cell
//     would have been named for something it did not observe.
//  2. THE PREMISE IS LOAD-BEARING. Making RemoveGrant a no-op — the revocation
//     simply not happening — turns this cell RED. So it observes the
//     revocation rather than some unrelated refusal the resumption would have
//     hit anyway.
//
// WHAT I COULD NOT DO, reported rather than glossed: I could not construct a
// mutation of the specific gate that refuses. Four attempts were mis-aimed —
// the obvious candidates (resolveUnitPolicy in WireExecutePortal, and again in
// wireExtEntry) are NOT the producer; instrumenting the !v.Standing branch and
// classifyGateError's ErrDenied case showed neither fires on this path. The
// gate that does fire is somewhere I have not located. The two measurements
// above are what this cell rests on, and a reviewer should treat the absence of
// a gate-level mutation as an open thread rather than as covered.
func TestPGF4_AuthorityIsReresolvedAtAPortalResumption(t *testing.T) {
	l := pgLoopFull(t)
	ctx := context.Background()
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)

	// An explicit transaction so the suspended portal survives between the two
	// Executes; an implicit one ends and takes the portal with it.
	if msgs := query(t, fe, "BEGIN"); hasError(msgs) {
		t.Fatalf("opening the transaction: %v", errorText(msgs))
	}

	fe.Send(&pgproto3.Parse{Name: "pg", Query: "SELECT g FROM generate_series(1,5) g"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pp", PreparedStatement: "pg"})
	fe.Send(&pgproto3.Execute{Portal: "pp", MaxRows: 2})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	first := readUntil(t, conn, fe, untilReady)

	// CONTROL: the first Execute SUCCEEDED and suspended. Without this the
	// refusal below could be a portal that never worked, and the cell would
	// report a re-resolution it never observed.
	rows := 0
	for _, m := range first {
		if _, ok := m.(*pgproto3.DataRow); ok {
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("control: %d rows on the first Execute, want 2; frames=%v", rows, kindsOf(first))
	}
	if _, ok := firstOfType[*pgproto3.PortalSuspended](first); !ok {
		t.Fatalf("control: no PortalSuspended, so there is no suspended portal to resume; frames=%v",
			kindsOf(first))
	}
	if hasError(first) {
		t.Fatalf("control: the first Execute errored: %v", errorText(first))
	}

	// THE REVOCATION, between the two Executes — the moment the row is about.
	if err := l.svc.RemoveGrant(ctx, l.rootTok, l.patUserID, l.connID, "127.0.0.1"); err != nil {
		t.Fatalf("revoking the grant: %v", err)
	}

	// THE RESUMPTION. It is an Execute, so the row contracts that it is
	// re-authorized — and the grant is gone.
	fe.Send(&pgproto3.Execute{Portal: "pp", MaxRows: 0})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	second := readUntil(t, conn, fe, untilReady)

	if !hasError(second) {
		delivered := 0
		for _, m := range second {
			if _, ok := m.(*pgproto3.DataRow); ok {
				delivered++
			}
		}
		t.Fatalf("the resumption returned %d more rows AFTER the grant was revoked, with no "+
			"refusal.\n\nThe matrix contracts authority re-resolved at EVERY Execute, "+
			"portal re-executions INCLUDED. A resumption riding the first Execute's authority "+
			"means revoking a grant does not stop a reading already in progress — it only stops "+
			"the next one, which is not what revoking a grant means.\nframes=%v",
			delivered, kindsOf(second))
	}
}
