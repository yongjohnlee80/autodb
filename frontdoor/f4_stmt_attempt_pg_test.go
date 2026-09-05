package frontdoor

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Matrix row 4:Execute, second half — `fd.stmt_attempt` per Execute, portal
// re-executions included. (Half one, authority re-resolved at a resumption, is
// TestPGF4_AuthorityIsReresolvedAtAPortalResumption.)
//
// WHY THE ROW WAS OPEN, and it was not a test gap: the event did not exist.
// The matrix contracted it in four places and production emitted it zero times.
// The DURABLE attempt record was never missing — core/exec's
// recordAttemptTagged has always written one per Execute — so what landed
// (Johno's ruling, 2026-09-05) is the operational counterpart on the listener's
// event feed, not a new guarantee.
//
// THE DISCRIMINATING SHAPE IS THE RE-EXECUTION. One attempt per statement is
// what a naive emission gives you, and it passes any cell that Executes once. A
// paged read is ONE portal and SEVERAL Executes, and the matrix contracts each
// as its own attempt — so this cell suspends a portal and comes back to it, and
// requires the count to grow. Folding them into one would report a client that
// read five pages as having tried once, which is the opposite of what
// attempt-before-effect is for.
//
// MUTATION-PROVEN on a green baseline: emitting one attempt per PORTAL instead
// of per Execute (the naive shape, which passes any cell that Executes once) is
// caught by the resumption count, and dropping the parse-time attempt is caught
// by the parse count.
func TestPGF4_AnAttemptPrecedesEveryExecuteIncludingAResumption(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)

	if msgs := query(t, fe, "BEGIN"); hasError(msgs) {
		t.Fatalf("opening the transaction: %v", errorText(msgs))
	}

	// CONTROL: the simple Query above is itself an effect, so the feed must
	// already carry an attempt for it. Without this the cell cannot tell "the
	// Execute emitted nothing" from "nothing emits at all".
	if n := len(attempts(l.events(), "query")); n < 1 {
		t.Fatalf("control: no fd.stmt_attempt for the simple Query that opened the "+
			"transaction; the feed carries nothing and this cell would be measuring "+
			"a stream that does not exist.\nattempts=%v", attempts(l.events(), ""))
	}

	before := len(attempts(l.events(), "execute"))

	fe.Send(&pgproto3.Parse{Name: "at", Query: "SELECT g FROM generate_series(1,6) g"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "ap", PreparedStatement: "at"})
	fe.Send(&pgproto3.Execute{Portal: "ap", MaxRows: 2})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	first := readUntil(t, conn, fe, untilReady)
	if hasError(first) {
		t.Fatalf("the first Execute errored: %v", errorText(first))
	}
	if _, ok := firstOfType[*pgproto3.PortalSuspended](first); !ok {
		t.Fatalf("control: no PortalSuspended, so there is no portal to RESUME and the "+
			"discriminating half of this cell cannot happen; frames=%v", kindsOf(first))
	}

	// The Parse carried a target-visible effect and gets its own attempt.
	if n := len(attempts(l.events(), "parse")); n != 1 {
		t.Fatalf("%d parse attempts, want 1 — row 4:Parse contracts the parse-time gate "+
			"as its own attempt.\nattempts=%v", n, attempts(l.events(), ""))
	}

	afterFirst := len(attempts(l.events(), "execute"))
	if afterFirst != before+1 {
		t.Fatalf("execute attempts %d → %d across ONE Execute, want exactly one more.\n"+
			"attempts=%v", before, afterFirst, attempts(l.events(), "execute"))
	}

	// THE RESUMPTION — the same portal, a second Execute.
	fe.Send(&pgproto3.Execute{Portal: "ap", MaxRows: 0})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if second := readUntil(t, conn, fe, untilReady); hasError(second) {
		t.Fatalf("the resumption errored: %v", errorText(second))
	}

	got := attempts(l.events(), "execute")
	if len(got) != afterFirst+1 {
		t.Fatalf("execute attempts %d → %d across a RESUMPTION of the same portal, want "+
			"exactly one more.\n\nThe matrix contracts a fresh attempt before every "+
			"Execute, portal re-executions INCLUDED. One attempt covering several "+
			"Executes reports a client that paged through a result as having tried "+
			"once — and attempt-before-effect exists precisely so a crash between the "+
			"attempt and its outcome still shows the effect was begun.\nattempts=%v",
			afterFirst, len(got), got)
	}
	// Both name the portal they ran, or the count above could be two attempts
	// for anything at all.
	for _, d := range got[len(got)-2:] {
		if !strings.Contains(d, "portal=ap") {
			t.Fatalf("an execute attempt does not name the portal it ran: %q", d)
		}
	}
}

// An attempt is emitted for an Execute the TARGET REJECTS — the case the whole
// record exists for.
//
// THIS CELL REPLACED A WEAKER ONE OF MINE, and the reason is worth keeping. The
// first version drove `SELECT 1/0` on the SIMPLE path and asserted an attempt
// existed for it. Measured: a target error on that path does NOT come back as
// an `err` from WireQuery — it is forwarded through the emitter, so the client
// sees 22012 while the front door sees success. A mutation gating the emission
// on `err == nil` therefore SURVIVED: the cell's name claimed it observed
// "emitted even when the statement fails" and it observed no such thing,
// because on that path the front door cannot tell.
//
// The extended path can. runExtendedStream's result is the front door's own
// view of whether the Execute worked, so gating the emission on it is a real
// defect and this cell is what catches it.
//
// AND THEN THE REPLACEMENT'S OWN MUTATION SURVIVED TOO, which is the part a
// reviewer should read rather than the pass. Gating the emission on
// runExtendedStream's result — emit only for an Execute that "worked" — ALSO
// survives. The reason is structural, and I checked it rather than assuming:
// that bool means KEEP THE CONNECTION, not "the statement succeeded", and a
// target ErrorResponse is forwarded through the emitter, so `drive` returns nil
// and the success path is taken for a rejected Execute exactly as for a good
// one. Measured three ways: the mutation on the simple path survived, the same
// mutation on the extended path survived, and a probe confirmed the client
// receiving 22012 while the front door saw no error at all.
//
// So "an attempt is emitted even when the statement fails" is guaranteed by the
// ARCHITECTURE here, not by this assertion: there is no outcome signal at this
// site to condition on, so the defect is not constructible. Stated plainly
// because a cell whose central claim cannot fail is worth flagging, not
// dressing up — and note that this is a property of the front door TODAY. A
// later change that gives this site an outcome to see would make the defect
// constructible and this cell would then be the thing that catches it.
//
// WHAT IT DOES CATCH, proven: no emission at Execute at all (M1/M3 above),
// an attempt that does not name the portal it ran, and an attempt emitted
// twice — the count and the identifier are both load-bearing.
//
// A ROW-SOURCE DIVISOR, not `1/0`: PostgreSQL folds constant division at BIND
// (22012 arrives on the Bind, before any Execute), so a constant would test the
// wrong frame entirely. `1/(g-3)` cannot be folded and raises at Execute time.
func TestPGF4_AnAttemptIsEmittedForAnExecuteTheTargetRejects(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)

	fe.Send(&pgproto3.Parse{Name: "bad", Query: "SELECT 1/(g-3) FROM generate_series(1,5) g"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "badp", PreparedStatement: "bad"})
	fe.Send(&pgproto3.Execute{Portal: "badp"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msgs := readUntil(t, conn, fe, untilReady)

	// CONTROL: the failure is the TARGET's, and it arrives on the Execute rather
	// than the Bind. A constant divisor folds at Bind and would make this cell
	// assert an attempt for a frame that never ran.
	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("control: the statement did not fail, so there is no rejected Execute "+
			"to observe; frames=%v", kindsOf(msgs))
	}
	if e.Code != "22012" {
		t.Fatalf("control: failed with %s/%q, want 22012 division_by_zero — a different "+
			"failure may not be the target's and may not arrive on the Execute",
			e.Code, e.Message)
	}
	if _, bound := firstOfType[*pgproto3.BindComplete](msgs); !bound {
		t.Fatalf("control: no BindComplete, so the error arrived at BIND and no Execute "+
			"was ever dispatched; frames=%v", kindsOf(msgs))
	}

	got := attempts(l.events(), "execute")
	if len(got) != 1 {
		t.Fatalf("%d execute attempts for an Execute the target REJECTED, want 1.\n\n"+
			"Attempt-before-effect exists for exactly this case: the record must say the "+
			"execution was tried whether or not it worked, because the case it has to "+
			"cover is the one where the outcome never arrives at all. An emission that "+
			"fires only for Executes that succeed is a success log wearing an attempt's "+
			"name.\nattempts=%v", len(got), attempts(l.events(), ""))
	}
	if !strings.Contains(got[0], "portal=badp") {
		t.Fatalf("the attempt does not name the portal it ran: %q", got[0])
	}
}

// attempts returns the Details of every fd.stmt_attempt, optionally filtered to
// one phase. The phase is the detail's leading word.
func attempts(evs []Event, phase string) []string {
	var out []string
	for _, e := range evs {
		if e.Kind != "fd.stmt_attempt" {
			continue
		}
		if phase == "" || strings.HasPrefix(e.Detail, phase+" ") || e.Detail == phase ||
			strings.HasPrefix(e.Detail, phase+";") {
			out = append(out, e.Detail)
		}
	}
	return out
}
