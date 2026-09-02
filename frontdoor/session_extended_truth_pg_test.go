package frontdoor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// lector r1 MF3 — the not-executed explanation must REACH A CLIENT.
//
// The engine can produce a perfectly truthful arm that no client will ever see.
// `reportOutputWithheld` treats the engine's report as the only snapshot, valid
// or not: an invalid TxStatus there means "the phase is unknown", §6.3 forbids
// inventing a readiness for it, and the loop closes without sending anything. So
// an extended producer that filled the arm correctly and left the status byte at
// zero got a silent session-loss where the client was owed an explanation.
//
// THE CELLS THAT MISSED IT asserted the EmitStopped fields in core/exec. They
// were right about every field and never drove the seam that shows them to
// anyone, which is the same shape as the #57 defects this commit fixes: assert
// what the audit recorded AND what the client received, because one can be right
// while the other is silent.
//
// The cap is tiny so the front door's own budget stops the output on the very
// first frame the segment produces — the earlier object's ErrorResponse.
func TestPGExt_TheNotExecutedExplanationReachesTheClient(t *testing.T) {
	addr, secret, database, eng := pgLoopWithEngine(t)
	_ = addr
	_ = eng
	// SIZED, NOT GUESSED. The cap must stop the SEGMENT's output and not the
	// follow-up SELECT's, or the follow-up trips it too and the cell reports a
	// desynchronised stream that is really its own fixture. The missing table's
	// name is long on purpose: it makes the target's 42P01 comfortably larger
	// than `SELECT 42`'s three small frames, so one cap separates them.
	cap := int64(120)
	_, _, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})

	fe := pgClient(t, listenAddr, secret, database)
	// The first object fails AT THE TARGET (42P01), which aborts the segment; the
	// second is a real, non-empty statement the target then discards.
	fe.Send(&pgproto3.Parse{Name: "bad",
		Query: "SELECT * FROM a_table_that_does_not_exist_and_whose_name_is_long_enough_to_size_the_error_x"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pbad", PreparedStatement: "bad"})
	fe.Send(&pgproto3.Parse{Name: "good", Query: "SELECT 4242"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "pgood", PreparedStatement: "good"})
	fe.Send(&pgproto3.Execute{Portal: "pgood"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// EXACTLY ONE readiness ends this segment, and it is SYNC's. The r2 version
	// of this cell stopped reading at the first one it saw, which is precisely
	// how it missed that there were two: the withheld report sent one and the
	// client's pipelined Sync sent the segment's real one. A client reads the
	// second as the answer to whatever it sends NEXT.
	var (
		gate    *pgproto3.ErrorResponse
		readies int
		extra   []string
	)
	for range 32 {
		m, err := fe.Receive()
		if err != nil {
			// The pre-MF3 behaviour: the connection closes and the client is told
			// nothing at all.
			t.Fatalf("reading the segment's answer: %v — the front door closed without telling the client "+
				"anything. A truthful arm the client cannot be shown is not a truthful answer", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok && e.Detail == ruleOutputCap {
			gate = e
			continue
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			readies++
			break
		}
		extra = append(extra, fmt.Sprintf("%T", m))
	}
	if gate == nil {
		t.Fatal("no output-cap ErrorResponse arrived; this cell proves nothing about the not-executed explanation")
	}
	if readies != 1 {
		t.Fatalf("readiness frames before the next command = %d, want 1 (other frames: %v)", readies, extra)
	}

	// THE WORDING IS THE CLAIM. "The query was empty" is what this said before
	// ArmNotExecuted existed, to a client that had just sent real SQL.
	if strings.Contains(gate.Message, "the query was empty") {
		t.Fatalf("the client was told its query was EMPTY: %q — it sent a statement, "+
			"and an earlier one in the same segment is what discarded it", gate.Message)
	}
	if !strings.Contains(gate.Message, "an earlier statement in this batch failed") {
		t.Fatalf("message = %q, want the not-executed lead", gate.Message)
	}
	if !strings.Contains(gate.Hint, "it has no effects") {
		t.Fatalf("hint = %q, want the not-executed effects clause", gate.Hint)
	}

	// THE STREAM IS SYNCHRONISED, proven by the NEXT command getting its own
	// answer rather than a leftover byte. A second readiness on the wire is
	// consumed here as though it were this SELECT's, and the client sees a
	// finished cycle where its rows should be. Draining a duplicate instead of
	// asserting its absence would hide exactly that.
	fe.Send(&pgproto3.Query{String: "SELECT 42"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var got string
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the following SELECT: %v", err)
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			t.Fatalf("the SELECT's first terminal arrived before its rows (row seen: %q) — it consumed a "+
				"readiness left over from the extended segment, so the stream is desynchronised", got)
		}
		if dr, ok := m.(*pgproto3.DataRow); ok && len(dr.Values) == 1 {
			got = string(dr.Values[0])
			break
		}
	}
	if got != "42" {
		t.Fatalf("the following SELECT returned %q, want 42", got)
	}
}
