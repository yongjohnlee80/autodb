package frontdoor

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// THE EXTENDED-PROTOCOL CLIENT SHAPES (matrix §10's pgx-class suite), built to
// white-vision's F4 fixture spec.
//
// Four matrix rows were AWAITING for one reason — "no live witness drives this
// frame end to end" — and no amount of engine-level celling substitutes for a
// client putting the frames on a real socket. These four close them.
//
// The spec's central warning governs every cell here: everything on this path
// RELAYS, so almost everything "runs". A cell asserting only that a segment
// completed proves the relay forwarded something, not that it forwarded the
// right thing. Each cell below names the ONE autodb behaviour it pins.

// sendSegment writes frames and flushes, then reads until the terminator, with a
// bound so a stranded segment fails instead of hanging.
func readUntil(t *testing.T, fe *pgproto3.Frontend, stop func(pgproto3.BackendMessage) bool) []pgproto3.BackendMessage {
	t.Helper()
	var out []pgproto3.BackendMessage
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("segment did not terminate; frames so far: %v", kindsOf(out))
		}
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment after %v: %v", kindsOf(out), err)
		}
		out = append(out, snapshot(msg))
		if stop(msg) {
			return out
		}
	}
}

func untilReady(m pgproto3.BackendMessage) bool {
	_, ok := m.(*pgproto3.ReadyForQuery)
	return ok
}

// SHAPE D — closes matrix row 4:Describe.
//
// THE ONE THING: the ParameterDescription and RowDescription carry the SERVER'S
// type OIDs, not a re-derivation. A producer that re-encoded would report text
// OID 25 for everything; the discriminator is asking for columns whose types
// differ from each other and from text.
func TestPGExtended_DescribeCarriesTheServersOwnOIDs(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	fe := pgClient(t, addr, secret, database)

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT $1::int AS n, $2::bool AS b, 'x'::text AS t"})
	fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "s"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s",
		Parameters: [][]byte{[]byte("7"), []byte("t")}})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "p"})
	fe.Send(&pgproto3.Execute{Portal: "p"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msgs := readUntil(t, fe, untilReady)

	pd, ok := firstOfType[*pgproto3.ParameterDescription](msgs)
	if !ok {
		t.Fatalf("no ParameterDescription for Describe('S'); frames=%v", kindsOf(msgs))
	}
	// int4 = 23, bool = 16. A re-deriving producer would say 25 (text).
	if len(pd.ParameterOIDs) != 2 || pd.ParameterOIDs[0] != 23 || pd.ParameterOIDs[1] != 16 {
		t.Fatalf("parameter OIDs = %v, want [23 16] — these are the SERVER's types, and text (25) "+
			"here would mean the front door re-derived them rather than forwarding what the "+
			"target said", pd.ParameterOIDs)
	}

	rd, ok := firstOfType[*pgproto3.RowDescription](msgs)
	if !ok {
		t.Fatalf("no RowDescription for Describe('P'); frames=%v", kindsOf(msgs))
	}
	if len(rd.Fields) != 3 {
		t.Fatalf("%d result fields, want 3", len(rd.Fields))
	}
	got := []uint32{rd.Fields[0].DataTypeOID, rd.Fields[1].DataTypeOID, rd.Fields[2].DataTypeOID}
	if got[0] != 23 || got[1] != 16 || got[2] != 25 {
		t.Fatalf("result OIDs = %v, want [23 16 25]; three DIFFERENT types is the discriminator — "+
			"a re-encoding producer collapses them to text", got)
	}
}

// SHAPE D, second half: NoData is its own frame, not an empty RowDescription.
func TestPGExtended_AStatementWithNoResultColumnsDescribesAsNoData(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	fe := pgClient(t, addr, secret, database)

	table := fmt.Sprintf("fd_nodata_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		c := pgClient(t, addr, secret, database)
		_ = query(t, c, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	fe.Send(&pgproto3.Parse{Name: "ins", Query: fmt.Sprintf("INSERT INTO %s VALUES (1)", table)})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pi", PreparedStatement: "ins"})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "pi"})
	fe.Send(&pgproto3.Execute{Portal: "pi"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msgs := readUntil(t, fe, untilReady)

	if _, ok := firstOfType[*pgproto3.NoData](msgs); !ok {
		t.Fatalf("Describe('P') on a statement with no result columns must answer NoData, a "+
			"DIFFERENT frame from an empty RowDescription — a client distinguishes them; frames=%v",
			kindsOf(msgs))
	}
	if rd, ok := firstOfType[*pgproto3.RowDescription](msgs); ok {
		t.Fatalf("an empty RowDescription (%d fields) was sent where NoData belongs", len(rd.Fields))
	}
}

// SHAPE E — narrows the Execute row of §4; it does NOT close it, and this
// comment deliberately avoids the citation form, because a cell that cites a
// row is claiming to witness it and this one does not.
//
// THE ONE THING: PortalSuspended REACHES THE CLIENT. A producer that swallows it
// strands a paging client forever — it waits for rows that will not come because
// it never learned it must ask again.
//
// WHAT THIS CELL DOES NOT PROVE, so nobody reads the row as closed: the matrix
// contracts authority re-resolved at EVERY Execute, "portal re-executions
// included", and an fd.stmt_attempt per Execute. A resumption riding the first
// Execute”'s authority would pass this cell and everything else written today —
// the existing live proof revokes a grant between Parse and Execute, which is
// not a re-execution. white-vision named the discriminating shape: revoke
// BETWEEN the first Execute and the resumption. Neither half is observable from
// here (attempt rows are engine-side audit, not listener events), which is why
// the row stays awaiting rather than being promoted on this cell.
func TestPGExtended_ARowLimitedFetchSuspendsAndResumes(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	fe := pgClient(t, addr, secret, database)

	// INSIDE AN EXPLICIT TRANSACTION, deliberately. A suspended portal does not
	// survive the end of an implicit one, so the natural way to drive this is
	// Execute/Flush/Execute — and a standalone Flush delivers nothing today (see
	// TestPGExtended_AStandaloneFlushDeliversNothing). BEGIN gives the portal a
	// transaction to live in, so this cell can narrow the Execute row's open half
	// without waiting on that defect.
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
	first := readUntil(t, fe, untilReady)

	rows := 0
	for _, m := range first {
		if _, ok := m.(*pgproto3.DataRow); ok {
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("%d rows for MaxRows=2; frames=%v", rows, kindsOf(first))
	}
	if _, ok := firstOfType[*pgproto3.PortalSuspended](first); !ok {
		t.Fatalf("no PortalSuspended after a row-limited Execute — a client paging through a "+
			"portal waits for it to learn it must ask again, so a producer that swallows it "+
			"strands the client forever; frames=%v", kindsOf(first))
	}

	// THE RESUMPTION IS STILL AN EXECUTE. The frame relay is the easy half; that
	// autodb treats a resumption as its own execution is the autodb-specific
	// claim, and it is what keeps a resumed portal re-authorized rather than
	// riding the first Execute's authority.
	fe.Send(&pgproto3.Execute{Portal: "pp", MaxRows: 0})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	rest := readUntil(t, fe, untilReady)
	more := 0
	for _, m := range rest {
		if _, ok := m.(*pgproto3.DataRow); ok {
			more++
		}
	}
	if more != 3 {
		t.Fatalf("resumption returned %d rows, want the remaining 3; frames=%v", more, kindsOf(rest))
	}
	if _, ok := firstOfType[*pgproto3.CommandComplete](rest); !ok {
		t.Fatalf("a drained portal must end in CommandComplete; frames=%v", kindsOf(rest))
	}
	_ = query(t, fe, "COMMIT")
}

// SHAPE F — closes matrix row 4:discard.
//
// THE ONE THING: after a mid-segment error, EVERY frame but Sync and Terminate
// is discarded, Sync ends the discard, and the session is usable afterwards.
//
// VISION'S TRAP, and it decides what this cell actually drives: `SELECT 1/0`
// folds at plan time, so PostgreSQL raises 22012 at BIND, not Execute — the
// client sees ParseComplete then ErrorResponse with no BindComplete. A volatile
// divisor defers it to execution. The frame order is asserted so the cell says
// which case it drove rather than leaving it to be assumed.
func TestPGExtended_AMidSegmentErrorDiscardsThroughSync(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	fe := pgClient(t, addr, secret, database)

	// Volatile divisor: the error is raised at EXECUTE, not folded at Bind.
	fe.Send(&pgproto3.Parse{Name: "bad", Query: "SELECT 1/(random()*0)::int"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pb", PreparedStatement: "bad"})
	fe.Send(&pgproto3.Execute{Portal: "pb"})
	// Everything after the failure must be discarded, not answered.
	fe.Send(&pgproto3.Parse{Name: "after", Query: "SELECT 99"})
	fe.Send(&pgproto3.Query{String: "SELECT 98"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msgs := readUntil(t, fe, untilReady)

	if _, ok := firstOfType[*pgproto3.BindComplete](msgs); !ok {
		t.Fatalf("no BindComplete: the divisor folded at plan time after all, so this cell drove "+
			"a BIND-time error and not the Execute-time one it is written for; frames=%v",
			kindsOf(msgs))
	}
	if _, ok := firstOfType[*pgproto3.ErrorResponse](msgs); !ok {
		t.Fatalf("the failing Execute produced no error; frames=%v", kindsOf(msgs))
	}
	// The discarded frames must have produced NOTHING. A second ParseComplete or
	// any row from "SELECT 98" means a frame after the error was answered.
	parseCompletes, rowsAfter := 0, 0
	for _, m := range msgs {
		switch m.(type) {
		case *pgproto3.ParseComplete:
			parseCompletes++
		case *pgproto3.DataRow:
			rowsAfter++
		}
	}
	if parseCompletes != 1 {
		t.Fatalf("%d ParseCompletes: a Parse sent after the error was answered instead of "+
			"discarded; frames=%v", parseCompletes, kindsOf(msgs))
	}
	if rowsAfter != 0 {
		t.Fatalf("%d rows after the error: a Query sent mid-discard was executed; frames=%v",
			rowsAfter, kindsOf(msgs))
	}

	// Sync ended the discard, and the session survives it.
	if got := query(t, fe, "SELECT 42"); hasError(got) {
		t.Fatalf("the session did not survive discard-through-Sync: %v", errorText(got))
	}
}

// SHAPE G — the Flush behaviour that row 4 of the matrix contracts, and the
//
// This is a FINDING, pinned rather than reported in a commit message. Row
// the Flush row was awaiting a live witness ("F2 routes it to the segment, but no
// witness drives a client Flush frame"). Driving one shows a standalone Flush
// delivers NOTHING: after Parse/Bind/Flush the client receives zero frames and
// the audit records no statement events at all — only conn_open, tls_ok,
// auth_ok, session_open. Delivery happens on Sync; the segment is not dispatched
// on Flush.
//
// That matters to real clients rather than to a conformance checkbox: Flush is
// how a paging client gets its first page without ending the transaction, and
// how any client interleaves work in one segment. A client that sends
// Parse/Bind/Execute/Flush and waits — which the protocol entitles it to do —
// waits until it gives up. The session RECOVERS through a subsequent Sync, so
// this is a missing capability rather than a broken session; the cell asserts
// that half too, and it is what corrected my first, worse reading.
//
// NOT FIXED HERE: it is F2's dispatch and the boundary on this slice is cells
// only. Reported to jarvis and white-vision.
//
// The cell asserts the CURRENT behaviour so the finding cannot rot: it fails
// the day Flush starts working, and tells whoever sees it to restore the real
// witness below it.
func TestPGExtended_AStandaloneFlushDeliversNothing(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	conn, fe := pgClientWithConn(t, addr, secret, database)

	fe.Send(&pgproto3.Parse{Name: "f", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pf", PreparedStatement: "f"})
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// BOUNDED WITH A DEADLINE, NOT A GOROUTINE. The first version read in a
	// goroutine and kept sending from the test body — pgproto3.Frontend is not
	// safe for concurrent use, so that was a data race. It PASSED without -race
	// and failed with it, which is the clearest possible statement that a cell
	// can be wrong in a way only one of the two runs shows.
	if err := conn.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err == nil {
		t.Fatalf("a standalone Flush DELIVERED %T — Flush now works, which is the fix this cell "+
			"was waiting for. Delete it and restore the real witness: a standalone Flush must "+
			"deliver the queued answers without a Sync, and an EMPTY Flush must be a no-op rather "+
			"than the producer's empty-queue refusal forwarded to a correct client", msg)
	}
	if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
		t.Fatalf("a standalone Flush produced %v rather than silence — the finding this cell pins "+
			"has changed shape and the new behaviour needs chasing", err)
	}

	// THE SESSION RECOVERS THROUGH SYNC, so this is a missing capability rather
	// than a broken session — and getting that right took the cell correcting me.
	//
	// An earlier version of this cell read in a goroutine while the test body
	// kept sending. That was a data race (pgproto3.Frontend is not safe for
	// concurrent use), and under it the Sync appeared unanswered — so I recorded
	// "the segment is STUCK until the idle deadline", a severity the evidence did
	// not support. With the race removed the Sync IS answered. The cell asserted
	// both directions, so it caught my overstatement rather than preserving it.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("a Sync after a Flush was NOT answered (%v) — the session does not recover, which "+
			"is a worse finding than the one recorded here and needs reporting as such", err)
	}
}
