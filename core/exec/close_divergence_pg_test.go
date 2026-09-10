package exec

// THE STORE MUST NOT FORGET A STATEMENT THE TARGET STILL HAS.
//
// Diagnosed from a real failure: autodb could not use another autodb front door
// as a target. Authentication and TLS succeeded, and then the first schema call
// died with a relayed
//
//	ERROR: prepared statement "stmtcache_ad3a…" already exists (SQLSTATE 42P05)
//
// which reached the operator as a bare "internal error" two layers away.
//
// THE DIVERGENCE, AND WHY ITS DIRECTION MATTERS. WireCloseStatement dropped the
// statement from this end IMMEDIATELY and only QUEUED the Close to the target,
// where it is delivered at the client's next Flush or Sync. A target error
// earlier in the same segment starts the discard to Sync, PostgreSQL discards
// every queued frame including that Close, and the two ends part company:
//
//	this end   the statement is gone
//	the target the statement is still there
//
// The next Parse of that name is then admitted here, relayed, and answered
// 42P05 by the target. A client cannot recover from it, because nothing it can
// send will make this end believe the statement exists.
//
// The two divergences are NOT symmetric, which is the design point:
//
//	store has it, target does not   -> our own ErrUnknownStatement: clean, ours
//	target has it, store does not   -> a relayed 42P05 the client cannot clear
//
// THE FIRST VERSION OF THIS FIX WAS WRONG, and the existing suite caught it.
// It kept the record until the CloseComplete arrived, which breaks a legitimate
// and common pattern: a client with a statement cache Closes an evicted entry
// and Parses its replacement UNDER THE SAME NAME in one segment -- pgx does
// exactly this, in one pipeline -- and PostgreSQL serves it because it
// processes the frames in order. Refusing that Parse as a duplicate is what
// TestExtPG_NamedStatementIsReusedAndEvictedLikeACache calls "a cache could
// never replace an entry". The eager drop existed for a reason.
//
// So the two halves are separated. The NAME is freed at once, so a replacement
// Parse is admitted. The TARGET'S COPY is remembered in pendingCloses until the
// CloseComplete confirms it. And a Parse for a name with an unconfirmed Close
// RE-ISSUES that Close ahead of itself -- the sequence the client's own frames
// described before the discard swallowed one of them.
//
// These cells therefore assert the CONSEQUENCE the operator felt, not the
// representation: a re-Parse after a discarded Close must not be answered
// 42P05.
//
// pgx is the client that finds this and pgjdbc is not: pgx Closes evicted
// cached statements as a batched pipeline, which is exactly the shape a discard
// swallows, and autodb's own pool runs a cache-exercising query on EVERY
// checkout (dsn.go, pgPrepareConnVerify).

import (
	"context"
	"testing"
)

func TestExtPG_ADiscardedCloseDoesNotOrphanTheTargetsStatement(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	// 1. A statement that really exists on the target, confirmed.
	extPrepare(t, f, sid, userID, "keep", "SELECT 1")
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync after preparing: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, ok := s.ext.statements["keep"]; !ok {
		t.Fatal("positive control: the statement was not recorded, so this cell observes nothing")
	}

	// 2. A segment in which a TARGET error precedes the Close, so the Close is
	//    discarded. The error must come from the target, not from our own gate:
	//    an undefined table is classified cleanly here and refused there.
	if err := f.eng.WireParse(ctx, sid, userID, "bad",
		"SELECT * FROM autodb_no_such_table_9a7c", nil, testIP); err != nil {
		t.Fatalf("parse of the erroring statement was refused locally, so the target "+
			"never errors and this cell's premise fails: %v", err)
	}
	if err := f.eng.WireCloseStatement(ctx, sid, userID, "keep"); err != nil {
		t.Fatalf("close: %v", err)
	}
	var sawTargetError bool
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		if m.Kind == "ErrorResponse" {
			sawTargetError = true
		}
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !sawTargetError {
		t.Fatal("the target did not error, so the segment was never discarded and the " +
			"Close was delivered normally: this cell's premise failed")
	}

	// 3. THE MECHANISM. The name is free — a replacement Parse must be admitted —
	//    but the target's copy is remembered, because its Close was discarded.
	if _, ok := s.ext.statements["keep"]; ok {
		t.Error("the name is still occupied, so a client replacing a cached entry would " +
			"be refused; the eager drop is what makes Close-then-Parse work")
	}
	if !s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Error("the discarded Close was forgotten: nothing now remembers that the target " +
			"still holds this statement, and the next Parse will be relayed into 42P05")
	}

	// 4. THE CONSEQUENCE THE OPERATOR FELT, and the assertion that matters.
	//    Re-Parsing the name is what a client with a statement cache does. It
	//    must succeed, and it must NOT be answered 42P05 by the target.
	if err := f.eng.WireParse(ctx, sid, userID, "keep", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("re-Parse after a discarded Close was refused here: %v", err)
	}
	if _, serr := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			t.Errorf("the target answered %q (SQLSTATE %s) — this is the production failure: "+
				"a name this end believed free, relayed to a target that still had it",
				m.Err.Message, m.Err.Code)
		}
		return nil
	}); serr != nil {
		t.Fatalf("sync after re-parse: %v", serr)
	}
	// And the repair is spent, not sticky.
	if s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Error("the pending close survived the repair, so every later Parse of this name " +
			"would re-issue a Close it does not need")
	}
}

// AND A CONFIRMED CLOSE LEAVES NOTHING BEHIND — the way this fix could itself
// be wrong.
//
// The pending record exists so a discarded Close is repaired. If it were never
// cleared on the ordinary path, every Parse of a once-closed name would re-issue
// a Close forever: harmless on the wire, but an unbounded list on the session
// and a repair that fires when nothing is broken.
func TestExtPG_AConfirmedCloseLeavesNoPendingRepair(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	extPrepare(t, f, sid, userID, "gone", "SELECT 1")
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync after preparing: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	retainedHeld := s.ext.retained

	if err := f.eng.WireCloseStatement(ctx, sid, userID, "gone"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !s.ext.closeUnconfirmed(objectStatement, "gone") {
		t.Fatal("positive control: no pending close was recorded, so this cell cannot " +
			"observe it being cleared")
	}

	var closeComplete bool
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		if m.Kind == "CloseComplete" {
			closeComplete = true
		}
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !closeComplete {
		t.Fatal("no CloseComplete reached the client; this cell cannot tell a confirmed " +
			"close from an unconfirmed one")
	}
	if s.ext.closeUnconfirmed(objectStatement, "gone") {
		t.Error("the pending close survived its own CloseComplete: the repair would fire " +
			"on every later Parse of this name, and the list grows without bound")
	}
	if len(s.ext.pendingCloses) != 0 {
		t.Errorf("%d pending close(s) remain after confirmation", len(s.ext.pendingCloses))
	}
	// The charge went back with the name, at the drop.
	if s.ext.retained >= retainedHeld {
		t.Errorf("retained state = %d, was %d while the statement was held: the drop "+
			"released the name but not the charge", s.ext.retained, retainedHeld)
	}
}

// THE REPAIR IS INVISIBLE TO THE CLIENT, and it is the finding
// that mattered most.
//
// The repair queues a real Close ahead of the Parse, and the drain forwards
// every target response. So the first version put an unsolicited CloseComplete
// in front of the client's ParseComplete — and a pipelining client tracks
// expected response types IN ORDER, so that desynchronizes its
// request/response pairing. The client this repair exists for is precisely such
// a client, which would have made the fix worse than the defect.
//
// This asserts the EXACT client-visible sequence, not merely that a
// ParseComplete arrives.
func TestExtPG_TheRepairEmitsNothingTheClientDidNotAskFor(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	// Leave the target holding "keep" with its Close discarded.
	extPrepare(t, f, sid, userID, "keep", "SELECT 1")
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := f.eng.WireParse(ctx, sid, userID, "bad",
		"SELECT * FROM autodb_no_such_table_9a7c", nil, testIP); err != nil {
		t.Fatalf("premise: %v", err)
	}
	if err := f.eng.WireCloseStatement(ctx, sid, userID, "keep"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Fatal("positive control: no pending repair, so no repair will run and this cell " +
			"asserts nothing")
	}

	// The repair runs inside this Parse. Record every frame the client sees.
	if err := f.eng.WireParse(ctx, sid, userID, "keep", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	var seen []string
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		seen = append(seen, m.Kind)
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// EXACTLY the client's own frame's answer, and nothing else.
	want := []string{"ParseComplete"}
	if len(seen) != len(want) || seen[0] != want[0] {
		t.Errorf("the client saw %v, want %v — a frame it did not send produced a frame it "+
			"can see, which is what desynchronizes a pipelining client", seen, want)
	}
	for _, k := range seen {
		if k == "CloseComplete" {
			t.Error("an unsolicited CloseComplete reached the client ahead of its " +
				"ParseComplete: the repair must consume its own answer")
		}
	}
}

// AND THE REPAIR SURVIVES A SECOND DISCARD.
//
// The pending record used to be cleared when the repair was QUEUED. If a later
// frame in that same segment errors, PostgreSQL discards the repair Close and
// the Parse together, the target still holds the old statement, and with the
// record already gone the next Parse no longer repairs it. The failure returns,
// silently, one segment later.
func TestExtPG_ADiscardedRepairStillRepairsOnTheNextParse(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	extPrepare(t, f, sid, userID, "keep", "SELECT 1")
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Discard #1: the Close is swallowed.
	if err := f.eng.WireParse(ctx, sid, userID, "bad1",
		"SELECT * FROM autodb_no_such_table_9a7c", nil, testIP); err != nil {
		t.Fatalf("premise: %v", err)
	}
	if err := f.eng.WireCloseStatement(ctx, sid, userID, "keep"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync: %v", err)
	}
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Fatal("positive control: nothing pending after the first discard")
	}

	// Discard #2. THE ERROR MUST COME FIRST IN THE SEGMENT, or the repair lands
	// and there is nothing to observe: PostgreSQL processes frames in order, so
	// a repair queued ahead of the erroring frame is delivered normally. The
	// first version of this cell queued them the other way round and passed for
	// the wrong reason.
	if err := f.eng.WireParse(ctx, sid, userID, "bad2",
		"SELECT * FROM autodb_no_such_table_9a7c", nil, testIP); err != nil {
		t.Fatalf("premise: %v", err)
	}
	if err := f.eng.WireParse(ctx, sid, userID, "keep", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, discardEmit); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Fatal("the pending repair was lost when its own segment was discarded: the target " +
			"still holds the statement and the next Parse will be relayed into 42P05")
	}

	// Third attempt, clean segment: the repair must still fire and succeed.
	if err := f.eng.WireParse(ctx, sid, userID, "keep", "SELECT 1", nil, testIP); err != nil {
		t.Fatalf("third re-Parse: %v", err)
	}
	if _, err := f.eng.WireSyncSegment(ctx, sid, userID, func(m WireMessage) error {
		if m.Kind == "ErrorResponse" && m.Err != nil {
			t.Errorf("the target answered %q (SQLSTATE %s) — the repair did not survive two "+
				"discards", m.Err.Message, m.Err.Code)
		}
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if s.ext.closeUnconfirmed(objectStatement, "keep") {
		t.Error("the pending repair survived a delivered repair Close")
	}
}
