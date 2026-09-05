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
// AND THE PREMISE IS NOW GUARDED, not merely disclosed (gold-man's suggestion,
// PR #85 r0): TestPGF4_ATargetErrorIsNotAFrontDoorFailure asserts that a target
// error leaves the session usable, which is WHY this site has no success signal.
// It goes red the day someone gives runExtendedStream a real verdict — the same
// day the defect becomes constructible and this cell starts doing real work.
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

// The `query` phase is PER BUFFER on the feed, and the durable record is per
// statement. Matrix note 1.3a says so; this is what checks it.
//
// FOUND BY GOLD-MAN IN REVIEW (PR #85 r0), in the commit whose stated purpose
// was to stop the document asserting something untrue — and it was the same
// shape: note 1.3a's first version read "one per statement of a simple Query",
// which is false. The front door emits once with the whole buffer as its
// detail; the split into statements happens inside WireQuery, downstream of
// the call.
//
// WHY THE ASYMMETRY STAYS rather than being fixed by emitting per statement:
// the parts only exist inside core/exec, and threading a per-statement emit
// seam through WireQuery is a large change to buy granularity on a
// best-effort feed. The sink that MUST be per statement — the one an
// investigation reads — already is: recordAttemptTagged runs per part
// (wire_query.go:348/367/398, inside the `for i, part := range parts` loop).
//
// SO THIS CELL EXISTS BECAUSE A COMMENT ASSERTING A RELATION IS NOT A
// MECHANISM. A paragraph in the matrix claiming "one event, three rows" is
// prose; nothing checked it, and the previous prose was wrong. The control is
// what makes the count mean per-buffer rather than per-anything: one statement
// must also produce exactly one, or "1 for 3 statements" would be satisfied by
// an emission that fires once per session, once per connection, or once ever.
func TestPGF4_TheQueryAttemptIsPerBufferAndTheDurableRecordIsPerStatement(t *testing.T) {
	l := pgLoopFull(t)
	fe := pgClient(t, l.addr, l.secret, l.database)

	before := len(attempts(l.events(), "query"))
	if msgs := query(t, fe, "SELECT 1; SELECT 2; SELECT 3"); hasError(msgs) {
		t.Fatalf("control: the three-statement buffer errored, so this cell is not "+
			"observing a buffer that ran: %v", errorText(msgs))
	}
	got := attempts(l.events(), "query")
	if n := len(got) - before; n != 1 {
		t.Fatalf("%d query attempts for a THREE-statement buffer, want exactly 1.\n\n"+
			"Note 1.3a states the feed is per BUFFER and the durable record is per "+
			"STATEMENT. If this is now %d, the emission moved and the note must move "+
			"with it — including the matrix row's Audit cell.\nattempts=%v", n, n, got)
	}
	// The detail carries the WHOLE buffer, which is what makes one event usable:
	// an event naming only the first statement would report the buffer as
	// something the client never sent.
	last := got[len(got)-1]
	for _, want := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if !strings.Contains(last, want) {
			t.Fatalf("the single attempt does not carry the whole buffer (missing %q): %q",
				want, last)
		}
	}

	// CONTROL: one statement is also exactly one. Without it the assertion above
	// is satisfied by an emission that fires once per session or once per
	// connection rather than once per buffer.
	mid := len(attempts(l.events(), "query"))
	if msgs := query(t, fe, "SELECT 9"); hasError(msgs) {
		t.Fatalf("control: the single-statement query errored: %v", errorText(msgs))
	}
	if n := len(attempts(l.events(), "query")) - mid; n != 1 {
		t.Fatalf("control: %d query attempts for ONE statement, want 1 — the count above "+
			"is not measuring per-buffer granularity", n)
	}
}

// THE PREMISE, GUARDED rather than merely disclosed — gold-man's suggestion on
// PR #85 r0, and it is the better shape.
//
// TestPGF4_AnAttemptIsEmittedForAnExecuteTheTargetRejects discloses that its
// central claim cannot fail: gating the emission on success is not
// constructible because this site has no outcome signal to gate on. That
// disclosure is honest but inert — nothing notices the day the premise stops
// holding.
//
// This is the assertion about the PREMISE: a target error is NOT a front-door
// failure. The connection survives it and the session stays usable, which is
// exactly why runExtendedStream's bool cannot serve as a success signal. Give
// that site a real success/failure verdict and this cell goes red on the same
// day the defect becomes constructible — which is the day the sibling cell
// starts doing real work.
//
// Asserted on what a CLIENT can see, not on the bool: the bool is internal and
// a cell reading it would be testing the implementation of the premise rather
// than the premise. A session that answers a fresh statement after a rejected
// Execute is the observable form of "the wire was not lost".
func TestPGF4_ATargetErrorIsNotAFrontDoorFailure(t *testing.T) {
	l := pgLoopFull(t)
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)

	fe.Send(&pgproto3.Parse{Name: "prem", Query: "SELECT 1/(g-3) FROM generate_series(1,5) g"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "premp", PreparedStatement: "prem"})
	fe.Send(&pgproto3.Execute{Portal: "premp"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msgs := readUntil(t, conn, fe, untilReady)

	e, ok := firstOfType[*pgproto3.ErrorResponse](msgs)
	if !ok {
		t.Fatalf("control: the Execute did not fail, so there is no target error whose "+
			"survivability this cell could observe; frames=%v", kindsOf(msgs))
	}
	if e.Code != "22012" {
		t.Fatalf("control: %s/%q, want 22012 — a front-door refusal instead of a target "+
			"error would make this cell assert the wrong thing", e.Code, e.Message)
	}

	// THE PREMISE: the session survived the target's error and still answers.
	after := query(t, fe, "SELECT 42")
	if hasError(after) {
		t.Fatalf("the session did not survive a target error: %v\n\n"+
			"This is the premise that TestPGF4_AnAttemptIsEmittedForAnExecuteTheTargetRejects "+
			"rests on. A target error is a normal protocol outcome, forwarded to the "+
			"client, NOT a front-door failure — which is why runExtendedStream's bool "+
			"means keep-the-connection and cannot be read as 'the statement worked'. If "+
			"this is now red, that site HAS an outcome signal, gating the emission on it "+
			"is a constructible defect, and the sibling cell's disclosure must become an "+
			"assertion.", errorText(after))
	}
	if _, ok := firstOfType[*pgproto3.DataRow](after); !ok {
		t.Fatalf("the follow-up statement returned no rows, so the session is not "+
			"demonstrably usable; frames=%v", kindsOf(after))
	}
}
