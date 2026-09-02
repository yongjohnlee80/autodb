package frontdoor

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
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

// §7 :386 — a segment that accumulates past its caps before Sync is refused,
// discards through Sync, and the connection stays.
//
// The counter is what this asserts, not the 96 MiB byte cap: driving 96 MiB
// through a cell would trade minutes of gate time for the same branch.
func TestExtCaps_ASegmentPastItsMessageCapIsRefusedAndDiscardsThroughSync(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}}
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	// One past the cap. Describe is the cheapest frame that still counts.
	for range maxSegmentMessages + 1 {
		fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "s"})
	}
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	var refusal *pgproto3.ErrorResponse
	for range 64 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's refusal: %v", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			refusal = e
			break
		}
	}
	if refusal == nil {
		t.Fatal("the segment cap never fired")
	}
	if refusal.Code != sqlStateConfiguredLimit || refusal.Detail != ruleSegmentCap {
		t.Fatalf("refusal = %s/%s, want %s/%s", refusal.Code, refusal.Detail,
			sqlStateConfiguredLimit, ruleSegmentCap)
	}

	// DISCARDING, then Sync ends it and the connection is still usable — a cap
	// refuses the segment, never the session.
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readUntilReadySoft(t, fe); got != txStatusIdle {
		t.Fatalf("readiness after Sync = %q, want %q", got, txStatusIdle)
	}
	r := runQueryOnce(t, fe)
	if r == nil {
		t.Fatal("the session was unusable after a segment-cap refusal; the cap must refuse the segment, not the session")
	}
}

// THE DISCRIMINATOR: is the re-admission defect FIXED, or merely UNREACHABLE?
//
// The shape and the reasoning are mine; the driver is white-vision's, and the
// defect is hers to have found. When the delivery boundary landed the existing
// discard-through-Sync cell went green — and a green integration cell is NOT
// evidence that re-admission is impossible: a boundary that keeps frames out of
// reach hides the defect rather than fixing it, and "fixed" and "unreachable"
// are different facts about a component.
//
// So this looks at what ONLY re-admission produces. PostgreSQL answers an error
// mid-segment by discarding until Sync and SAYING NOTHING FURTHER, so the count
// of ErrorResponses before readiness is the discriminator — one is conformant,
// many is the bug — and no reader-side change can alter that count either way.
//
// DRAINED THROUGH READINESS, which is the step the cap cell above skips: it
// breaks at the first ErrorResponse and therefore cannot count a second. That
// one-step-short shape is how this defect sat behind a green suite.
//
// The byte cap trips it rather than the message cap: same branch, four frames
// instead of ten thousand, and no dependence on the default staying where it is.
// One driver is enough BECAUSE it is one branch — a second cell through the
// message cap would fail with this one, for this one's reason.
func TestAdmission_ADiscardingSegmentIsNotReAdmittedFrameByFrame(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParameterDescription"}, {Kind: "NoData"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20 // out of the way: the BYTE cap is what trips here
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	// Four frames, each over the cap on its own, then Sync — in ONE write, so
	// they are pipelined behind the breach exactly as a real client sends them.
	big := rawDescribe(strings.Repeat("b", 5000))
	one := []byte{}
	for range 4 {
		one = append(one, big...)
	}
	if _, err := conn.Write(append(one, rawSync()...)); err != nil {
		t.Fatal(err)
	}

	refusals, sawReady := 0, false
	for !sawReady {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("after %d refusals and no readiness: %v — the segment desynchronised", refusals, err)
		}
		switch v := m.(type) {
		case *pgproto3.ErrorResponse:
			if v.Detail != ruleSegmentCap {
				t.Fatalf("refusal = %s/%s, want %s", v.Code, v.Detail, ruleSegmentCap)
			}
			refusals++
		case *pgproto3.ReadyForQuery:
			sawReady = true
		}
	}
	if refusals != 1 {
		t.Fatalf("%d ErrorResponses before readiness, want exactly 1 — PostgreSQL answers an error "+
			"mid-segment by discarding until Sync and saying nothing further, so one refusal per "+
			"over-cap frame means the segment is being RE-ADMITTED while already discarding", refusals)
	}
}

// AND THE FRAMES BEHIND THE BREACH ARE SKIPPED, NOT DECODED — MEASURED.
//
// This cell exists because a mutation found nothing. Dropping fr.skipFrame(h)
// from the guard above, leaving everything else intact, kept the whole package
// GREEN: the refusal count is right, readiness arrives, the session recovers.
// Every visible behaviour is identical, because the difference is not a
// behaviour — it is what the refusal COST.
//
// white-vision flagged the same line from the other side when she found her fix
// could not carry the skip on main. Between the two, the half neither of us
// could see was the half nothing observed.
//
// MEASURED, both directions: 5 bytes reach the Backend with the skip — the Sync
// that ends the discard, and nothing else — against 15026 without it, the three
// discarded bodies decoded in full. The segment cap is 96 MiB and the message
// cap is now 64 MiB, so "decoded anyway" is the whole cost of the refusal.
func TestAdmission_FramesDiscardedBehindABreachAreNotDecoded(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParameterDescription"}, {Kind: "NoData"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20
	var mu sync.Mutex
	var readers []*frameReader
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
		testReaderReady: func(fr *frameReader) { mu.Lock(); readers = append(readers, fr); mu.Unlock() },
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	mu.Lock()
	if len(readers) == 0 {
		mu.Unlock()
		t.Fatal("no session reader was published; this cell cannot measure anything")
	}
	fr := readers[len(readers)-1]
	mu.Unlock()
	before := fr.deliveredBytes()

	big := rawDescribe(strings.Repeat("b", 5000))
	one := []byte{}
	for range 4 {
		one = append(one, big...)
	}
	if _, err := conn.Write(append(one, rawSync()...)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("draining the refused segment: %v", err)
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	// The Sync is the only frame in that write the front door is entitled to
	// decode: it is what ENDS the discard. Everything ahead of it was refused.
	const syncWire = 5
	if got := fr.deliveredBytes() - before; got > syncWire {
		t.Errorf("%d bytes reached the Backend while a segment was discarding, want at most %d (the Sync) "+
			"— frames behind a breach must be SKIPPED, and a discard that decodes what it discards pays "+
			"the full cost of the frames it refused", got, syncWire)
	}
}

// ...and the same property at the UNIT, which is the half an integration cell
// cannot be trusted for.
//
// Driving admitSegmentFrame directly is independent of what any reader delivers,
// so it separates "the component is correct" from "the component is currently
// unreachable" — the distinction the cell above cannot make on its own once a
// delivery boundary sits upstream of it. Both are white-vision's, written
// against a different repair of the same defect before this one existed, which
// is the one thing a cell written alongside a fix cannot be.
func TestAdmission_AnAlreadyDiscardingSegmentIsNotAdmittedAgain(t *testing.T) {
	t.Parallel()
	l, conn, be, fr, refusals := admissionHarness(t)
	closeReason := ""
	seg := &segmentLane{}
	over := frameHeader{typ: 'D', declared: int(maxSegmentBytes) + 1}

	if !l.admitSegmentFrame(conn, be, fr, seg, over, "probe", &closeReason) {
		t.Fatal("the first breach closed the session; it must refuse the segment and survive")
	}
	if !seg.discarding {
		t.Fatal("the first breach did not set discarding; this cell cannot observe re-admission")
	}
	msgs, bytes := seg.msgs, seg.bytes

	if !l.admitSegmentFrame(conn, be, fr, seg, over, "probe", &closeReason) {
		t.Fatal("the second frame closed the session")
	}
	if seg.msgs != msgs || seg.bytes != bytes {
		t.Errorf("a discarding segment was re-counted: msgs %d→%d, bytes %d→%d", msgs, seg.msgs, bytes, seg.bytes)
	}
	if got := refusals(); got != 1 {
		t.Errorf("the client received %d ErrorResponses for ONE segment breach, want 1", got)
	}
}

// admissionHarness gives a Listener, a live Backend over a real pipe, the reader
// the admission path now takes, and a count of the ErrorResponses actually
// WRITTEN to the peer — which is the assertion that matters, since a re-refusal
// is a defect precisely because the client sees it.
func admissionHarness(t *testing.T) (*Listener, net.Conn, *pgproto3.Backend, *frameReader, func() int) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	seen := make(chan int, 1)
	go func() {
		fe := pgproto3.NewFrontend(client, client)
		n := 0
		for {
			m, err := fe.Receive()
			if err != nil {
				seen <- n
				return
			}
			if _, ok := m.(*pgproto3.ErrorResponse); ok {
				n++
			}
		}
	}()

	l, _, _ := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
	})
	fr := newFrameReader(server)
	return l, server, pgproto3.NewBackend(fr, server), fr, func() int {
		_ = server.Close()
		_ = client.Close()
		select {
		case n := <-seen:
			return n
		case <-time.After(5 * time.Second):
			t.Fatal("the frontend reader did not finish")
			return -1
		}
	}
}

// runQueryOnce sends one simple Query and returns its first DataRow, or nil.
func runQueryOnce(t *testing.T, fe *pgproto3.Frontend) *pgproto3.DataRow {
	t.Helper()
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			return nil
		}
		if dr, ok := m.(*pgproto3.DataRow); ok {
			return dr
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			return nil
		}
	}
	return nil
}

// Row 4:Sync (:273) — Sync RESETS the segment counters.
//
// Without the reset the counters accumulate across segments, so a long-lived
// session is eventually refused for frames it had already had answered. The
// failure would look like the cap working, which is why it needs its own cell:
// two segments each comfortably under the cap must both be admitted.
func TestExtCaps_SyncResetsTheSegmentCounters(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}}
	_, addr := loopListener(t, q)
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	// Two segments, each just under the cap. Their SUM is well over it, so this
	// passes only if Sync cleared the count.
	const perSegment = maxSegmentMessages - 1
	for segment := range 2 {
		for range perSegment {
			fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "s"})
		}
		fe.Send(&pgproto3.Sync{})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		for range perSegment * 4 {
			m, err := fe.Receive()
			if err != nil {
				t.Fatalf("segment %d: %v", segment+1, err)
			}
			if e, ok := m.(*pgproto3.ErrorResponse); ok {
				t.Fatalf("segment %d of %d frames was refused (%s/%s) — each segment is under the cap, "+
					"so the counters carried across the Sync that should have cleared them",
					segment+1, perSegment, e.Code, e.Detail)
			}
			if _, ok := m.(*pgproto3.ReadyForQuery); ok {
				break
			}
		}
	}
}

// §7 :386's BYTE half, and the property that makes a byte cap mean anything:
// THE CHARGE IS NEVER LESS THAN WHAT ARRIVED.
//
// The first version of this cap reconstructed a frame's size from the DECODED
// message and under-charged a NULL-parameter Bind by 3x on the wire and ~12.5x
// on what it made the front door hold — 16,389 charged against 49,165 sent
// (lector C r1 MF2, measured). A cap on an under-estimate does not bound what it
// claims to, and it fails in the direction that admits. The charge is now the
// DECLARED wire length, taken from frameReader before the body is decoded, plus
// the decoded delta.
//
// The cell asserts the inequality rather than a figure, so it keeps holding if
// the delta is retuned: whatever the front door charges, it may not be less than
// the bytes the peer actually sent.
func TestExtCaps_TheByteChargeIsNeverLessThanTheWireLength(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20 // out of the way: only the BYTE cap may fire here
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// A Describe of a known size: 4 length bytes + the object-type byte + the
	// name and its NUL. Nothing is estimated here — this is what goes on the wire.
	name := strings.Repeat("n", 64)
	perFrame := int64(4 + 1 + len(name) + 1)

	// EXACTLY ENOUGH FRAMES to put the WIRE total just past the cap — one more
	// than fits. If the charge tracks the wire, this trips; if the charge is any
	// under-estimate, it does not, and the cell fails by timing out waiting for a
	// refusal that never comes. That is the decisive shape: the old estimator
	// would have charged these ~16 bytes each and admitted them all.
	frames := int(capBytes/perFrame) + 1
	sent := int64(frames) * perFrame
	for range frames {
		fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: name})
	}
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var refusal *pgproto3.ErrorResponse
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("no refusal after %d bytes on the wire against a %d-byte cap (%v) — "+
				"the charge is under the wire length", sent, capBytes, err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			refusal = e
			break
		}
	}
	if refusal == nil {
		t.Fatalf("the byte cap never fired for %d bytes against a %d-byte cap", sent, capBytes)
	}
	if refusal.Detail != ruleSegmentCap {
		t.Fatalf("refusal detail = %q, want %q", refusal.Detail, ruleSegmentCap)
	}

	// THE INEQUALITY, from the refusal's own event: what was charged is at least
	// the cap it broke, and the wire total that produced it was under one frame
	// more than the cap — so the charge cannot have been less than what arrived.
	charged := int64(-1)
	for _, ev := range events() {
		if ev.Kind == "fd.refused" && ev.Reason == ruleSegmentCap {
			var msgs int
			var b int64
			if _, err := fmt.Sscanf(ev.Detail, "segment reached %d messages / %d bytes before Sync", &msgs, &b); err == nil {
				charged = b
			}
		}
	}
	if charged < 0 {
		t.Fatal("no segment-cap refusal event carried a byte figure")
	}
	if charged <= capBytes {
		t.Fatalf("charged %d against a %d-byte cap — the refusal fired without the charge exceeding it", charged, capBytes)
	}
	// The upper bound allows §1.5's STAGE TWO — the decoded pre-allocation, which
	// is a real charge and is per frame — but not an unbounded one: an
	// over-charge that outgrew the wire would refuse correct clients well before
	// the documented cap.
	const generousDeltaPerFrame = 64
	if limit := sent + int64(frames)*generousDeltaPerFrame + perFrame; charged > limit {
		t.Fatalf("charged %d for %d bytes on the wire across %d frames (limit %d) — the charge has outgrown "+
			"what arrived by more than the decoded delta can account for", charged, sent, frames, limit)
	}
}

// The shape that refuted the estimate: 8192 NULL parameters, which carry no
// payload bytes and were therefore priced at almost nothing.
//
// On the wire this is roughly 32 KiB of length words, and the decode holds a
// slice header per parameter. Under a 4 KiB cap it must be refused by the FIRST
// frame — the old estimator charged it 5 bytes and admitted thousands.
func TestExtCaps_ANullParameterBindIsChargedWhatItActuallyCosts(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParseComplete"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	params := make([][]byte, 8192) // every one nil: a NULL, four wire bytes, no payload
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s", Parameters: params})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the Bind's answer: %v", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			if e.Detail != ruleSegmentCap {
				t.Fatalf("refusal detail = %q, want %q", e.Detail, ruleSegmentCap)
			}
			return
		}
	}
	t.Fatalf("a Bind of %d NULL parameters was admitted under a %d-byte cap — it carries no payload, "+
		"which is exactly why an estimate built from the decoded message priced it at nothing",
		len(params), capBytes)
}

// rawDescribe builds a Describe('S') exactly as it goes on the wire, so a cell
// can put several frames in ONE socket write — which is the shape read-ahead
// turns into a mis-attribution.
func rawDescribe(name string) []byte {
	body := 1 + len(name) + 1 // object type, name, NUL
	out := make([]byte, 0, 5+body)
	out = append(out, 'D')
	out = binary.BigEndian.AppendUint32(out, uint32(4+body))
	out = append(out, 'S')
	out = append(out, name...)
	return append(out, 0)
}

func rawSync() []byte {
	return append([]byte{'S'}, 0, 0, 0, 4)
}

// lector C r2 MF2 — ONE socket write carrying [Describe, Sync, Describe].
//
// frameReader's scan frames all three before Receive returns the first, so a
// design that charged a live segment as each was framed put the THIRD frame's
// bytes into the FIRST frame's segment. Two failures out of one read: the small
// compliant Describe was refused for bytes it did not send, and the Sync in
// between then reset the counters, erasing the charge the large Describe had
// already been given — so the frame that should have been refused was admitted.
//
// The client's view is what makes it observable. A compliant first Describe
// produces no output of its own, so the FIRST thing that arrives must be the
// Sync's readiness. An ErrorResponse before it is the false refusal.
func TestExtCaps_ReadAheadInOneWriteChargesEachFrameToItsOwnSegment(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParameterDescription"}, {Kind: "NoData"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	small := rawDescribe(strings.Repeat("a", 90)) // ~97 bytes: well under the cap
	big := rawDescribe(strings.Repeat("b", 5000)) // ~5007 bytes: over it, alone
	one := append(append(append([]byte{}, small...), rawSync()...), big...)
	if _, err := conn.Write(one); err != nil {
		t.Fatal(err)
	}

	// The Sync's readiness must arrive BEFORE any refusal: the first Describe is
	// compliant, and the segment it belongs to ended at that Sync.
	first, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the first segment's answer: %v", err)
	}
	if e, ok := first.(*pgproto3.ErrorResponse); ok {
		t.Fatalf("the COMPLIANT first Describe was refused (%s/%s) — the second Describe's bytes were charged "+
			"to the segment the first belongs to, which read-ahead makes possible and framing order prevents",
			e.Code, e.Detail)
	}
	for {
		if _, ok := first.(*pgproto3.ReadyForQuery); ok {
			break
		}
		if first, err = fe.Receive(); err != nil {
			t.Fatalf("waiting for the first segment's readiness: %v", err)
		}
	}

	// And the SECOND segment, which the oversized Describe is alone in, must be
	// refused — its charge cannot have been erased by the Sync that preceded it.
	for range 8 {
		m, rerr := fe.Receive()
		if rerr != nil {
			t.Fatalf("the oversized Describe after the Sync was ADMITTED — its charge was erased by the reset "+
				"that belongs to the segment before it (%v)", rerr)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			if e.Detail != ruleSegmentCap {
				t.Fatalf("refusal = %s/%s, want %s", e.Code, e.Detail, ruleSegmentCap)
			}
			return
		}
	}
	t.Fatal("no refusal for the oversized Describe in its own segment")
}

// lector C r2 MF3 — the queue must survive the AUTH boundary.
//
// Auth shares this reader and its Backend. A post-auth frame can arrive in the
// same socket write as the password and be framed while auth is still running,
// so its header exists before the session loop does. A design that only began
// accounting when the loop installed something never charged that frame at all —
// the first frame of the session, which is a good one to be unable to refuse.
func TestExtCaps_AFrameReadAheadDuringAuthIsStillCharged(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParameterDescription"}, {Kind: "NoData"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := startupTo(t, addr, defaultParams())
	defer func() { _ = conn.Close() }()
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// ONE write: the password, and behind it a Describe over the cap.
	pw := []byte("good")
	pass := append([]byte{'p'}, 0, 0, 0, byte(4+len(pw)+1))
	pass = append(append(pass, pw...), 0)
	big := rawDescribe(strings.Repeat("b", 5000))
	if _, err := conn.Write(append(pass, big...)); err != nil {
		t.Fatal(err)
	}

	sawReady := false
	for range 24 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("after auth: %v", err)
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok && !sawReady {
			sawReady = true // the auth sequence's own readiness
			continue
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			if e.Detail != ruleSegmentCap {
				t.Fatalf("refusal = %s/%s, want %s", e.Code, e.Detail, ruleSegmentCap)
			}
			return
		}
	}
	t.Fatalf("the oversized Describe that arrived in the same write as the password was ADMITTED — "+
		"its header was framed while auth held the reader, and nothing charged it (cap %d)", capBytes)
}

// §1.5 STAGE TWO decides on its own, and this is the only cell where it does.
//
// A frame can be comfortably under the cap on the WIRE and still make the front
// door hold far more than it sent: a NULL parameter is four bytes of length word
// and a whole slice header once decoded. Stage one admits such a frame; stage
// two is what refuses it, and without a cell whose refusal only stage two can
// produce, that branch is unwitnessed — the mutation that disables it stays
// green because stage one is doing all the refusing everywhere else.
func TestExtCaps_TheDecodedDeltaRefusesAFrameThatFitsOnTheWire(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "BindComplete"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// 900 NULL parameters: about 3.6 KiB declared — UNDER the 4 KiB cap, so
	// stage one admits it — but 900 slice headers once decoded, which is not.
	params := make([][]byte, 900)
	fe.Send(&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s", Parameters: params})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("a Bind that fits on the wire but not in memory was ADMITTED (%v) — stage two is "+
				"what refuses it, and nothing else can", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok {
			if e.Detail != ruleSegmentCap {
				t.Fatalf("refusal = %s/%s, want %s", e.Code, e.Detail, ruleSegmentCap)
			}
			return
		}
	}
	t.Fatal("no refusal for a frame whose decoded size crosses the cap its wire size did not")
}
