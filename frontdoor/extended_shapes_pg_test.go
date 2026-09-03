package frontdoor

import (
	"crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// THE EXTENDED-PROTOCOL CLIENT SHAPES (matrix §10's pgx-class suite), built to
// white-vision's F4 fixture spec.
//
// Four matrix rows were AWAITING for one reason — "no live witness drives this
// frame end to end" — and no amount of engine-level celling substitutes for a
// client putting the frames on a real socket.
//
// TWO of them close here (4:Describe, 4:discard). 4:Execute is NARROWED, not
// closed: the row also contracts resumption re-authorization and a per-Execute
// attempt row, neither of which these cells witness. 4:Flush is witnessed
// ABSENT — the behaviour is not there, and the cell pins the defect rather than
// the contract.
//
// The spec's central warning governs every cell here: everything on this path
// RELAYS, so almost everything "runs". A cell asserting only that a segment
// completed proves the relay forwarded something, not that it forwarded the
// right thing. Each cell below names the ONE autodb behaviour it pins.

// sendSegment writes frames and flushes, then reads until the terminator, with a
// bound so a stranded segment fails instead of hanging.
func readUntil(t *testing.T, conn *tls.Conn, fe *pgproto3.Frontend,
	stop func(pgproto3.BackendMessage) bool) []pgproto3.BackendMessage {
	t.Helper()
	// BOUNDED ON THE SOCKET (r0 MF1). A wall-clock check at the top of the loop
	// cannot fire while fe.Receive() is blocked in netFD.Read — the loop never
	// gets to look. This is the THIRD time tonight I wrote that shape (#66's
	// readiness drain, #65's two-frame cell, and here); the fix did not transfer
	// because each was a new helper and I re-derived the bound instead of reusing
	// the lesson.
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("bounding the segment read: %v", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var out []pgproto3.BackendMessage
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment after %v: %v — a stranded segment must fail here "+
				"promptly, not hang to the go-test timeout", kindsOf(out), err)
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
	conn, fe := pgClientWithConn(t, addr, secret, database)

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
	msgs := readUntil(t, conn, fe, untilReady)

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
	conn, fe := pgClientWithConn(t, addr, secret, database)

	table := fmt.Sprintf("fd_nodata_%d", time.Now().UnixNano())
	if msgs := query(t, fe, fmt.Sprintf("CREATE TABLE %s (n int)", table)); hasError(msgs) {
		t.Fatalf("creating the scratch table: %v", errorText(msgs))
	}
	t.Cleanup(func() {
		_, c := pgClientWithConn(t, addr, secret, database)
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
	msgs := readUntil(t, conn, fe, untilReady)

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
// here — attempt rows are engine-side audit, not listener events, so a cell
// would have to open its own meta-store handle; awkward and the wrong home
// rather than impossible — which is why the row stays awaiting rather than
// being promoted on this cell.
func TestPGExtended_ARowLimitedFetchSuspendsAndResumes(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	conn, fe := pgClientWithConn(t, addr, secret, database)

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
	first := readUntil(t, conn, fe, untilReady)

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
	rest := readUntil(t, conn, fe, untilReady)
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
	conn, fe := pgClientWithConn(t, addr, secret, database)

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
	msgs := readUntil(t, conn, fe, untilReady)

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

// SHAPE G — a standalone Flush DELIVERS, which is what Flush is for.
//
// THIS REPLACES A CELL THAT PINNED THE DEFECT. Its predecessor asserted the
// opposite — that Parse/Bind/Flush delivered nothing until a Sync — and said in
// its own words that it "fails the day Flush starts working" and should be
// replaced by the real witness. That day is this change; this is that witness.
//
// It matters to real clients rather than to a conformance checkbox: Flush is how
// a paging client gets its first page without ending the transaction, and how
// any client interleaves work in one segment. A client that sends
// Parse/Bind/Execute/Flush and waits — which the protocol entitles it to do —
// waited until it gave up.
//
// TWO THINGS, because either alone can pass while the other is broken: the
// answers must ARRIVE without a Sync, and they must arrive BECAUSE OF THE FLUSH.
// The second is not pedantry — an Execute already drains the engine's segment
// into the backend's write buffer, so a cell that only checked "bytes arrived
// after I sent a Flush" cannot tell delivery from a buffer that some other path
// happened to write. The cell therefore proves silence BEFORE the Flush first.
func TestPGExtended_AStandaloneFlushDelivers(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled})
	conn, fe := pgClientWithConn(t, addr, secret, database)

	fe.Send(&pgproto3.Parse{Name: "f", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pf", PreparedStatement: "f"})
	fe.Send(&pgproto3.Execute{Portal: "pf"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// PREMISE — SILENCE BEFORE THE FLUSH. §5's fidelity rule: an Execute's output
	// is not sent until the client asks. Without this the cell cannot attribute
	// the delivery below to the Flush at all.
	//
	// BOUNDED WITH A DEADLINE, NOT A GOROUTINE. An earlier version of the cell
	// this replaces read in a goroutine while the body kept sending;
	// pgproto3.Frontend is not safe for concurrent use, so that was a data race
	// that PASSED without -race and failed with it.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if msg, err := fe.Receive(); err == nil {
		t.Fatalf("PREMISE FAILED: %T arrived before the client asked for it — the "+
			"cell can no longer tell delivery-at-Flush from output sent early", msg)
	}

	// NOW THE FLUSH. Everything that follows is attributable to it.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	var sawRow bool
	for i := 0; i < 8; i++ {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("a standalone Flush delivered %v and then stopped: %v — Flush "+
				"must dispatch the segment without a Sync", kinds, err)
		}
		kinds = append(kinds, strings.TrimPrefix(fmt.Sprintf("%T", m), "*pgproto3."))
		if dr, ok := m.(*pgproto3.DataRow); ok &&
			len(dr.Values) == 1 && string(dr.Values[0]) == "1" {
			sawRow = true
		}
		if _, done := m.(*pgproto3.CommandComplete); done {
			break
		}
	}
	// THE VALUE, not merely that frames arrived. A cell satisfied by "something
	// came back" is satisfied by a buffer draining, which is the failure its
	// predecessor was written to describe.
	if !sawRow {
		t.Errorf("the Flush delivered %v with no DataRow carrying 1 — the statement's "+
			"result did not reach the client", kinds)
	}

	// AN EMPTY FLUSH IS A NO-OP, not the producer's empty-queue refusal forwarded
	// to a correct client. A client that flushes defensively with nothing pending
	// is doing something the protocol allows.
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if msg, err := fe.Receive(); err == nil {
		if e, isErr := msg.(*pgproto3.ErrorResponse); isErr {
			t.Fatalf("an empty Flush was answered with ErrorResponse(%s) — a "+
				"defensive flush with nothing pending must be a no-op", e.Code)
		}
	}

	// The session is still usable, and the follow-up statement really RUNS.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = readUntil(t, conn, fe, untilReady)
	got := query(t, fe, "SELECT 42")
	if hasError(got) {
		t.Fatalf("the session was not usable after a Flush: %v", errorText(got))
	}
	dr, ok := firstOfType[*pgproto3.DataRow](got)
	if !ok || len(dr.Values) != 1 || string(dr.Values[0]) != "42" {
		t.Fatalf("the follow-up statement returned no row with 42 (frames=%v) — an "+
			"errorless reply proves the wire drained, not that a statement ran", kindsOf(got))
	}
}

// THE PUBLIC-BOUNDARY WITNESS for the stall discharge: a REAL engine, a REAL
// socket, and a client that collects its output with a Flush and then pauses
// well past the segment-stall budget.
//
// IT IS LIVE RATHER THAN FAKE-BACKED DELIBERATELY. The fake re-emits its scripted
// messages on Flush, so a fake-backed cell can be green because the FAKE
// delivered rather than because the code did — and what the code has to do here
// is write the socket. Only a real engine behind a real listener can tell those
// apart at the boundary a client actually observes.
//
// Execute asks for output; Sync and Flush collect it. Before the discharge
// existed, a client that Executed, Flushed and paused was torn down at the stall
// budget for output it had already taken.
func TestPGExtended_AFlushDischargesTheStallBudget(t *testing.T) {
	stall := 400 * time.Millisecond
	_, secret, database, eng := pgLoopWithEngine(t)
	_, events, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
		testSegmentStall: &stall,
	})
	conn, fe := pgClientWithConn(t, addr, secret, database)

	fe.Send(&pgproto3.Parse{Name: "d", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pd", PreparedStatement: "d"})
	fe.Send(&pgproto3.Execute{Portal: "pd"})
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// PREMISE: THE CLIENT ACTUALLY RECEIVED ITS OUTPUT. Without bytes on the
	// socket nothing was collected, and a cell asserting "the stall did not arm"
	// would be asserting that a STALLED client gets the idle clock — the exact
	// inverse of the rule it is written to protect.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var collected bool
	for i := 0; i < 8; i++ {
		m, err := fe.Receive()
		if err != nil {
			break
		}
		kinds = append(kinds, strings.TrimPrefix(fmt.Sprintf("%T", m), "*pgproto3."))
		if _, done := m.(*pgproto3.CommandComplete); done {
			collected = true
			break
		}
	}
	if !collected {
		t.Fatalf("PREMISE FAILED: the Flush delivered %v with no CommandComplete, so "+
			"nothing was collected and the stall budget SHOULD still be armed", kinds)
	}

	// Well past the budget. The client took its output; it owes nothing.
	time.Sleep(4 * stall)
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleSegmentStall {
			t.Fatal("the stall budget armed after a Flush had already delivered the " +
				"segment's output to the client")
		}
	}

	// And the session is still usable, with the follow-up statement really running.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session was torn down after collecting its own output: %v", err)
	}
	_ = readUntil(t, conn, fe, untilReady)
	got := query(t, fe, "SELECT 42")
	if hasError(got) {
		t.Fatalf("the session was not usable after Flush-then-pause: %v", errorText(got))
	}
	dr, ok := firstOfType[*pgproto3.DataRow](got)
	if !ok || len(dr.Values) != 1 || string(dr.Values[0]) != "42" {
		t.Fatalf("the follow-up statement returned no row with 42 (frames=%v)", kindsOf(got))
	}
}
