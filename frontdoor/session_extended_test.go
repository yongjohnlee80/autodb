package frontdoor

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE EXTENDED SEGMENT'S STALL BUDGET (jarvis's ruling, 2026-09-03).
//
// A client that queued an Execute, holds pending output, and has neither Synced
// nor Flushed is not idle — it is one that will not collect what it asked for.
// These three cells are the ruling's own list: the budget fires, it does not
// fire early, and it does not arm at all for a segment holding nothing.

func stallListener(t *testing.T, q QueryExecutor, stall time.Duration) (*Listener, func() []Event, string) {
	t.Helper()
	return listenerWith(t, Options{
		Authn:             &fakeAuth{result: goodSession()},
		Queries:           q,
		AuthFailuresPerIP: unthrottled,
		testSegmentStall:  &stall,
	})
}

func execSegment(t *testing.T, fe *pgproto3.Frontend) {
	t.Helper()
	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"})
	fe.Send(&pgproto3.Execute{Portal: "p"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
}

// A client that holds a segment open past the budget is torn down, the lane it
// held goes back, and the audit says SEGMENT-STALL rather than session-deadline.
//
// The lane assertion waits on inUse() reaching zero, never on a frame the client
// received: §5 item 3 measured 22 of 300 statements still holding their working
// set at the moment the client had already been told the statement finished, so
// readiness is not a proxy for an idle lane.
func TestExtStall_SilentSegmentIsTornDownAndReleasesTheLane(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}, {Kind: "BindComplete"}, {Kind: "CommandComplete", Tag: "SELECT 1"}}
	l, events, addr := stallListener(t, q, 300*time.Millisecond)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	execSegment(t, fe)

	// POSITIVE CONTROL: the segment took the WORKING SET, not merely some bytes.
	//
	// "> 0" is not enough and that is not a nicety — with no segment reservation
	// the accountant's oversized-frame path fires for every frame (any size beats
	// a held of zero) and tops up a few bytes each time, so a non-zero lane is
	// exactly what the BROKEN version produces. Measured: 16 bytes. Asserting the
	// watermark distinguishes a real pre-dispatch reservation from per-frame
	// top-ups, which is the whole difference the design turns on.
	want := l.outputWatermark()
	waitFor(t, "the segment to reserve its output working set", func() bool { return l.general.inUse() >= want })

	// ...and then the client says nothing at all.
	waitFor(t, "the stalled segment to be audited", func() bool {
		ev, ok := find(events(), "fd.refused")
		return ok && ev.Reason == ruleSegmentStall
	})
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleSessionDeadline {
			t.Fatal("the stall was audited as gate/session-deadline; that reason says the session was idle and it was not")
		}
	}
	waitFor(t, "the lane to be released", func() bool { return l.general.inUse() == 0 })
}

// The same segment, Synced INSIDE the budget, is not torn down — so the cell
// above is observing the budget and not merely a segment that always dies.
func TestExtStall_SyncWithinBudgetKeepsTheSession(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}, {Kind: "BindComplete"}, {Kind: "CommandComplete", Tag: "SELECT 1"}}
	l, events, addr := stallListener(t, q, 3*time.Second)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	execSegment(t, fe)
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q, want the engine's %q", got, txStatusIdle)
	}
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleSegmentStall {
			t.Fatal("a segment Synced inside its budget was torn down as stalled")
		}
	}
	// Sync released the reservation, so nothing is left holding the lane.
	waitFor(t, "the lane to be released by Sync", func() bool { return l.general.inUse() == 0 })
}

// A segment that has Parsed and Bound but executed NOTHING holds no lane, so the
// stall budget must not arm — that session is merely between messages and stays
// under the idle clock.
//
// Without this the ruling's narrowing would be untested and a Parse/Bind pair
// left open would be torn down in 300ms as though it were hoarding output.
func TestExtStall_SegmentHoldingNothingStaysUnderTheIdleClock(t *testing.T) {
	t.Parallel()
	q := okQueries()
	l, events, addr := stallListener(t, q, 250*time.Millisecond)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both frames to reach the engine", func() bool { return len(q.calls()) == 2 })
	if used := l.general.inUse(); used != 0 {
		t.Fatalf("a Parse/Bind-only segment holds %d lane bytes; it has produced no output", used)
	}

	// Well past the stall budget, and nothing happens: the idle clock governs.
	time.Sleep(4 * 250 * time.Millisecond)
	for _, e := range events() {
		if e.Kind == "fd.refused" && e.Reason == ruleSegmentStall {
			t.Fatal("the stall budget armed for a segment holding nothing")
		}
	}
	// The session is still usable.
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session was torn down: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("readiness = %q; the session did not survive its idle wait", got)
	}
}

// r0 MF3 — a front-door-LOCAL refusal must start discard-through-Sync.
//
// PostgreSQL ignores every frame but Sync and Terminate after an error in a
// segment, but the target only starts that when IT produced the error. A Parse
// refused at our gate never reaches the target, so nothing there starts it — and
// the frames the client already pipelined behind the failure would be executed as
// though the refusal had not happened.
func TestExtDiscard_LocalParseRefusalDiscardsUntilSync(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.parseErr = auth.ErrDenied
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "rejected", Query: "INSERT INTO t VALUES (1)"})
	fe.Send(&pgproto3.Parse{Name: "discarded", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "discarded"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness after the discarded segment (got %q); engine saw %v", got, q.calls())
	}
	got := q.calls()
	if len(got) != 2 || got[0] != "Parse:rejected:INSERT INTO t VALUES (1)" || got[1] != "Sync" {
		t.Fatalf("the engine saw %v, want the refused Parse and then only Sync — everything pipelined behind a "+
			"local refusal must be discarded", got)
	}
}

// ...and the same for a refusal at EXECUTE, which is the other place the front
// door refuses without the target ever seeing the statement (re-authorization).
//
// Both are needed: the Parse case discards before anything is bound, the Execute
// case discards after a statement and portal already exist, and a discard that
// only covered the first would let the second run the frames behind it.
func TestExtDiscard_LocalExecuteRefusalDiscardsUntilSync(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.executeErr = auth.ErrDenied
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "s", Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"})
	fe.Send(&pgproto3.Execute{Portal: "p"})
	fe.Send(&pgproto3.Close{ObjectType: 'S', Name: "s"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness after the discarded segment (got %q); engine saw %v", got, q.calls())
	}
	got := q.calls()
	if len(got) == 0 || got[len(got)-1] != "Sync" {
		t.Fatalf("the engine saw %v, want it to end at Sync", got)
	}
	for _, c := range got {
		if c == "CloseS:s" {
			t.Fatalf("the Close pipelined behind a refused Execute reached the engine (%v); "+
				"after a local refusal every frame but Sync is discarded", got)
		}
	}
}

// r1 MF4, loop level — a simple Query pipelined behind a local refusal must be
// discarded. The live cell proves the absent ROW; this one proves the engine is
// never asked, which is the same rule one layer up and costs no database.
func TestExtDiscard_SimpleQueryDoesNotEscapeTheDiscard(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.parseErr = auth.ErrDenied
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	fe.Send(&pgproto3.Parse{Name: "rejected", Query: "SELECT 1"})
	fe.Send(&pgproto3.Query{String: "INSERT INTO t VALUES (1)"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("no readiness after the discarded segment (got %q)", got)
	}
	// SNAPSHOT UNDER THE MUTEX. sawSQL is written on the SERVER goroutine and read
	// here; -race staying quiet would not establish a happens-before through
	// socket readiness, which is the accidental ordering that hid the same defect
	// in this fake once already.
	if ran := q.statements(); len(ran) != 0 {
		t.Fatalf("the engine executed %v inside a discarding segment; every message but Sync and Terminate "+
			"is dropped, and a simple Query is a message", ran)
	}

	// POSITIVE CONTROL: after Sync ended the discard, the same session runs a
	// simple Query normally — so the guard released rather than wedged.
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("the session did not recover after Sync (got %q)", got)
	}
	if ran := q.statements(); len(ran) != 1 || ran[0] != "SELECT 1" {
		t.Fatalf("after Sync the engine saw %v, want [SELECT 1] — the discard must end at Sync", ran)
	}
}
