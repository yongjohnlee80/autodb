package exec

import (
	"context"
	"slices"
	"testing"
	"time"
)

// SYNC MUST NOT DESTROY THE SEGMENT'S TAIL.
//
// A segment's answers are delivered by the Execute drive, which walks the queued
// steps and stops at its own terminal. Sync then ends the segment. Anything
// queued AFTER the last Execute's terminal — or in a segment carrying no Execute
// at all — therefore has no drive to deliver it, and Sync discards it along with
// the objects it created.
//
// The reachable case is a prepared statement: Parse, Describe, Sync is exactly
// what pgx does on its default exec mode and what database/sql's Prepare does on
// every mode. The statement is created, its answers are queued, no Execute
// follows, and Sync sweeps the object as unfinalized — so the client's next Bind
// is refused for a statement it just successfully created.
//
// §4a is explicit that this is backwards: portals do not survive the
// transaction, prepared statements DO.
func TestExtPG_APreparedStatementSurvivesSyncWithoutAnExecute(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "s1", "SELECT 1 AS n", nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := f.eng.WireDescribeStatement(ctx, sid, userID, "s1"); err != nil {
		t.Fatalf("describe: %v", err)
	}

	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}

	// PREMISE. If the segment were empty and the statement already absent here,
	// "gone after Sync" would be observing nothing and the cell would pass on a
	// vacuum. Both halves are asserted before the operation under test runs.
	if len(s.ext.segment) == 0 {
		t.Fatal("PREMISE FAILED: nothing is queued before Sync, so nothing could be discarded")
	}
	if _, serr := s.ext.statement("s1"); serr != nil {
		t.Fatalf("PREMISE FAILED: the statement is absent before Sync: %v", serr)
	}

	var got []WireMessage
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		got = append(got, m)
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// BOTH HALVES, because fixing either alone leaves a client broken in a way
	// the other half hides. Delivering nothing but keeping the statement lets
	// Prepare report zero result fields and still "work"; delivering the answers
	// and then destroying the statement refuses the next Bind.
	kinds := kindsOfMsgs(got)
	for _, want := range []string{"ParseComplete", "ParameterDescription", "RowDescription"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("Sync delivered %v, missing %s — the client cannot learn the "+
				"result shape of a statement it just prepared", kinds, want)
		}
	}

	if _, serr := s.ext.statement("s1"); serr != nil {
		t.Fatalf("the prepared statement did not survive Sync: %v — §4a keeps "+
			"prepared statements across the transaction; only portals go", serr)
	}
}

// The same rule with an Execute present, which is what makes it a rule about the
// TAIL rather than about segments that happen to lack an Execute. The Close is
// queued after the Execute's terminal, so the drive has already stopped by the
// time it is reached.
func TestExtPG_ASecondStatementSurvivesSyncAfterAnEarlierExecute(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One statement that runs to completion...
	if err := f.eng.WireParse(ctx, sid, userID, "ran", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("parse ran: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "ran", "ran", nil, nil, nil); err != nil {
		t.Fatalf("bind ran: %v", err)
	}
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "ran", 0, testIP,
		func(WireMessage) error { return nil }); err != nil {
		t.Fatalf("execute ran: %v", err)
	}

	// ...then a second statement prepared AFTER it, whose answers land in the
	// tail the drive has already walked past.
	if err := f.eng.WireParse(ctx, sid, userID, "tail", "SELECT 2", nil, testIP); err != nil {
		t.Fatalf("parse tail: %v", err)
	}

	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, serr := s.ext.statement("tail"); serr != nil {
		t.Fatalf("PREMISE FAILED: the tail statement is absent before Sync: %v", serr)
	}

	var got []WireMessage
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		got = append(got, m)
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if kinds := kindsOfMsgs(got); !slices.Contains(kinds, "ParseComplete") {
		t.Errorf("Sync delivered %v after an earlier Execute, missing the tail's "+
			"ParseComplete", kinds)
	}
	if _, serr := s.ext.statement("tail"); serr != nil {
		t.Fatalf("a statement prepared after an Execute did not survive Sync: %v", serr)
	}
}

// A SEGMENT CAN OWE ANSWERS WITH NOTHING IN FLIGHT.
//
// Transaction-control statements are answered by the front door itself, so their
// ParseComplete is queued ON THE STEP and nothing is sent to the target. The
// segment is then non-empty while the wire is idle — and the first cut of the
// tail delivery read "non-empty" as "flush it", which golib refuses with
// "nothing queued to flush".
//
// That refusal came back as a segment-ending error, and Sync's error path sends
// no ReadyForQuery, so the client was left waiting on a readiness that was never
// coming. A HANG, on a segment PostgreSQL answers normally. This cell exists
// because the sweep case that found it takes ten seconds to fail by timeout,
// which is not a signal anyone should have to wait for twice.
func TestExtPG_SyncDeliversASegmentWithNothingInFlight(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := f.eng.WireParse(ctx, sid, userID, "b", "BEGIN", nil, testIP); err != nil {
		t.Fatalf("parse BEGIN: %v", err)
	}
	if err := f.eng.WireBind(ctx, sid, userID, "b", "b", nil, nil, nil); err != nil {
		t.Fatalf("bind BEGIN: %v", err)
	}
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "b", 0, testIP,
		func(WireMessage) error { return nil }); err != nil {
		t.Fatalf("execute BEGIN: %v", err)
	}

	// A statement the front door answers itself: its ParseComplete is queued on
	// the step, and the target is sent nothing.
	if err := f.eng.WireParse(ctx, sid, userID, "sp", "SAVEPOINT sp1", nil, testIP); err != nil {
		t.Fatalf("parse SAVEPOINT: %v", err)
	}

	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	// PREMISE, both halves. The segment must be non-empty (or Sync has nothing to
	// deliver and the cell is vacuous) AND every step must be locally answered
	// (or the wire genuinely does owe something and this is not the shape at all).
	if len(s.ext.segment) == 0 {
		t.Fatal("PREMISE FAILED: the segment is empty, so this cell observes nothing")
	}
	if segmentAwaitsWire(s.ext.segment) {
		t.Fatal("PREMISE FAILED: a step awaits a wire answer, so this is not the " +
			"nothing-in-flight shape the cell is named for")
	}

	var got []WireMessage
	status, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("Sync failed on a segment with nothing in flight: %v — this error "+
			"reaches the client as a refusal with NO ReadyForQuery behind it, and the "+
			"client then waits for a readiness that never comes", err)
	}
	if !validExtTxStatus(status) {
		t.Errorf("Sync returned status %q, want a valid transaction status", string(status))
	}
	if kinds := kindsOfMsgs(got); !slices.Contains(kinds, "ParseComplete") {
		t.Errorf("Sync delivered %v, missing the locally-answered ParseComplete", kinds)
	}
}

// validExtTxStatus is the cell's own copy, deliberately: asserting against the
// engine's constant would pass for any byte the engine happened to return.
func validExtTxStatus(b byte) bool { return b == 'I' || b == 'T' || b == 'E' }

// discardEmit is for cells that assert Sync's STATUS or its error, not what it
// delivered. Sync answers the segment's tail now, so those cells need a sink;
// giving them one that drops frames keeps them testing what they were written
// to test.
func discardEmit(WireMessage) error { return nil }
