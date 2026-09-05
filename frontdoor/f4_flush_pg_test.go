package frontdoor

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// MATRIX ROW 4:Flush — a Flush DELIVERS what the segment has queued, before
// any Sync.
//
// This is the row's POSITIVE case, and until now nothing drove it. What the
// suite had was TestPGExtended_AStandaloneFlushDeliversNothing, which drives a
// Flush with an EMPTY segment — and an empty Flush is a documented protocol
// no-op (extended_lane_admission_test.go), so that cell witnesses correct
// behaviour rather than the contract. The row's own triage reason read the two
// as the same thing and recorded Flush as a missing capability whose witness
// was "to be restored when Flush dispatches"; dispatch has existed since
// session_extended.go routed it to WireFlushSegment. Both halves of that reason
// were stale, which is why this cell exists.
//
// THE CONTRACT, and why it matters to a real client: Parse, Bind and Describe
// produce no frame of their own — their answers are QUEUED and delivered when
// the client asks, exactly as PostgreSQL delivers them. A client that pipelines
// Parse/Bind/Describe and then Flushes is asking for those answers WITHOUT
// ending the segment. pgx does precisely this to learn a statement's parameter
// and row types before deciding what to send next. If Flush delivered nothing
// until Sync, such a client would block waiting for a description that only
// arrives once it gives up and Syncs — and it would look like a hung server.
func TestPGF4_FlushDeliversTheQueuedAnswersBeforeSync(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)
	_, _, addr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled,
	})
	fe := pgClient(t, addr, secret, database)

	// Parse / Bind / Describe, then FLUSH — and no Sync yet.
	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 4242 AS answer"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	fe.Send(&pgproto3.Describe{ObjectType: 'P', Name: "p1"})
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// THE THREE QUEUED ANSWERS must arrive on the Flush alone. Reading is
	// bounded so a front door that delivers nothing FAILS HERE with a
	// diagnosis rather than hanging until the test binary's own timeout,
	// which reports as an unrelated panic and buries the finding.
	var (
		got       []string
		parseOK   bool
		bindOK    bool
		described bool
		ready     bool
	)
	for range 8 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("after Parse/Bind/Describe/Flush the front door delivered nothing and the "+
				"read failed (%v).\n\nRow 4:Flush's contract is that a Flush delivers what the "+
				"segment has queued WITHOUT ending it. A client that pipelines a describe and "+
				"waits for it — pgx does, to learn parameter and row types — blocks until it "+
				"gives up and Syncs, and reports a hung server.\ngot so far: %v", err, got)
		}
		got = append(got, fmt.Sprintf("%T", m))
		switch m.(type) {
		case *pgproto3.ParseComplete:
			parseOK = true
		case *pgproto3.BindComplete:
			bindOK = true
		case *pgproto3.RowDescription, *pgproto3.NoData:
			described = true
		case *pgproto3.ReadyForQuery:
			ready = true
		}
		if described {
			break
		}
	}

	if !parseOK || !bindOK || !described {
		t.Fatalf("Flush did not deliver the queued answers.\n"+
			"  ParseComplete=%v BindComplete=%v Describe-answer=%v\n"+
			"  frames seen: %v", parseOK, bindOK, described, got)
	}

	// AND FLUSH MUST NOT END THE SEGMENT. This is the half that separates
	// Flush from Sync, and a front door that answered a Flush by finishing the
	// segment would satisfy every assertion above while breaking every
	// pipelining client: the readiness would be consumed as the answer to
	// whatever the client sends next.
	if ready {
		t.Errorf("a ReadyForQuery arrived on the FLUSH: %v\n"+
			"Flush delivers, Sync ends. A readiness here is read by the client as the answer "+
			"to its NEXT command, which desynchronises the stream.", got)
	}

	// The segment is still live: Execute then Sync completes normally, which
	// proves the Flush left a usable session rather than a drained one.
	fe.Send(&pgproto3.Execute{Portal: "p1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var rows, completes, readies int
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("completing the segment after a Flush: %v (frames: %v)", err, got)
		}
		switch m.(type) {
		case *pgproto3.DataRow:
			rows++
		case *pgproto3.CommandComplete:
			completes++
		case *pgproto3.ReadyForQuery:
			readies++
		}
		if readies > 0 {
			break
		}
	}
	if rows != 1 || completes != 1 || readies != 1 {
		t.Errorf("after the Flush the segment did not complete normally: rows=%d completes=%d "+
			"readies=%d — the Flush left the session in a state a client cannot finish from",
			rows, completes, readies)
	}
}
