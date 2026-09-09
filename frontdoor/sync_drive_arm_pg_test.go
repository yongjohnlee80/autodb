package frontdoor

// F2 ITEM 6: THE SYNC DRIVE ARMS, AND WHAT IT ARMS REACHES BOTH SURFACES.
//
// The item asked for the Flush/Sync drives to arm an exec.EmitStopped and for
// drive->wire and drive->audit witnesses. The prerequisite shipped earlier —
// armFromWhatIsKnown now yields to the emitter only on ArmUnresolved — and the
// drives themselves did not arm at all, so the precedence rule was unreachable
// from a segment-ending call.
//
// WHY ARMING NAIVELY WOULD HAVE BEEN WORSE THAN NOT ARMING. EmitStopped.TxStatus
// is documented as "the same byte the loop's readiness would carry", and
// reportOutputWithheld takes a non-nil report's status as its ONLY snapshot: an
// invalid one means the phase is unknown, matrix §6.3 forbids inventing a
// readiness, and the loop closes. A Flush-shaped arm carries 0 — a Flush does
// not end the segment, so there is no ReadyForQuery for it to carry — and 0 is
// not a valid status. Arming Flush would therefore DROP a session that was
// perfectly recoverable, because the client can still Sync.
//
// So the two drives are NOT symmetric, and that asymmetry is the finding:
//
//   - SYNC has a truthful byte and only Sync does. It consumes through the
//     terminal ReadyForQuery, and the drain keeps OBSERVING after delivery
//     stops, so its status is post-tail exactly as the raw path's is. It arms.
//   - FLUSH has no byte at all. It does not arm, and TestFlushDrive below pins
//     that as a decision rather than an omission.
//
// This cell drives a segment with NO EXECUTE — Parse/Describe/Sync, which is
// what pgx's default exec mode and database/sql's Prepare send on every mode.
// That is precisely the shape whose answers only the Sync drive delivers: the
// Execute drive stops at its own terminal, so a segment carrying no Execute has
// no other drive to deliver anything.

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestPGSyncDrive_TheArmReachesTheClientAndTheAudit(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)

	// Small enough that the Describe's answer is withheld on the first frame,
	// large enough that nothing else in the exchange trips it.
	cap := int64(64)
	_, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})

	fe := pgClient(t, listenAddr, secret, database)
	// NO EXECUTE. Parse, then Describe the statement, then Sync — the shape
	// whose answers only the Sync drive delivers.
	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1 AS one, 2 AS two, 3 AS three"})
	fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "s1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// DRIVE -> WIRE. The client must be TOLD, and must get exactly one
	// readiness — Sync's. Before the drive armed, a withheld tail on this shape
	// produced a silent close: the arm was correct and nobody could see it.
	var (
		gate    *pgproto3.ErrorResponse
		readies int
		ready   *pgproto3.ReadyForQuery
	)
	for range 32 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answer: %v — the front door closed without "+
				"telling the client anything. A truthful arm the client cannot be shown "+
				"is not a truthful answer, and that silent close is what item 6 is about", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok && e.Detail == ruleOutputCap {
			gate = e
			continue
		}
		if r, ok := m.(*pgproto3.ReadyForQuery); ok {
			readies++
			ready = r
			break
		}
	}
	if gate == nil {
		t.Fatal("the client was never told its output was withheld")
	}
	if readies != 1 {
		t.Fatalf("readiness frames = %d, want exactly 1 (Sync's own)", readies)
	}
	// THE BYTE IS A REAL STATUS, not the zero that would have meant
	// session-lost. Which of I/T/E it is depends on the target's phase; that it
	// is one of them at all is the property item 6 turns on.
	if !validTxStatus(ready.TxStatus) {
		t.Errorf("readiness carried %q, which is not a valid status — an arm with an "+
			"invalid byte is exactly the session-lost path the drive must not take",
			ready.TxStatus)
	}

	// The message must name the effects clause, which recordedEffects derives
	// from the ARM. A client that is told its output stopped and nothing about
	// its effects has been given half an answer.
	if !strings.Contains(gate.Message, "not fully delivered") {
		t.Errorf("the gate error does not say the result was not delivered: %q", gate.Message)
	}

	// DRIVE -> AUDIT. The operator's record must carry the same outcome the
	// client was given. One can be right while the other is silent — that is
	// the shape of the defects this whole area keeps producing.
	var outcome string
	for _, e := range events() {
		if e.Kind == "fd.stmt_outcome" && e.Reason == ruleOutputCap {
			outcome = e.Detail
		}
	}
	if outcome == "" {
		t.Fatalf("no fd.stmt_outcome audited under %s; the client was told and the "+
			"operator was not.\nevents=%v", ruleOutputCap, kinds(events()))
	}
	if !strings.Contains(outcome, "effects=") {
		t.Errorf("the audited outcome carries no effects clause: %q", outcome)
	}
}

// AND THE FLUSH DRIVE DELIBERATELY DOES NOT ARM.
//
// Pinned as a decision. A Flush does not end the segment, so there is no
// readiness byte for an arm to carry; the only value available is 0, which
// reportOutputWithheld treats as session-lost. A consumer that stopped reading
// mid-segment has NOT lost its session — it can still Sync — so arming here
// would destroy a recoverable connection to report a stop.
//
// The witness is that the session SURVIVES a withheld Flush: the client is told,
// and its own Sync afterwards still ends the segment normally. If Flush ever
// starts arming with a fabricated byte, the Sync below stops arriving.
func TestPGFlushDrive_AWithheldFlushDoesNotDropTheSession(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)

	cap := int64(64)
	_, _, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: eng, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})

	fe := pgClient(t, listenAddr, secret, database)
	fe.Send(&pgproto3.Parse{Name: "f1", Query: "SELECT 1 AS one, 2 AS two, 3 AS three"})
	fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "f1"})
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	// The Flush's own answer may be withheld — that is the point — but the
	// connection must still be usable afterwards.
	sawGate := false
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("the session died on a withheld FLUSH: %v — a mid-segment consumer "+
				"stop is recoverable, and dropping it is what arming Flush with an "+
				"invalid status would cause", err)
		}
		if e, ok := m.(*pgproto3.ErrorResponse); ok && e.Detail == ruleOutputCap {
			sawGate = true
			break
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			t.Fatal("a Flush produced a readiness byte; only Sync ends a segment")
		}
	}
	if !sawGate {
		t.Skip("the cap did not stop this Flush's output; the cell cannot show the " +
			"recovery it exists for without a withheld Flush")
	}

	// THE RECOVERY. The client's own Sync still ends the segment.
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the connection was unusable after a withheld Flush: %v", err)
	}
	for range 16 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("no readiness after the recovering Sync: %v — the session was "+
				"dropped by the Flush after all", err)
		}
		if r, ok := m.(*pgproto3.ReadyForQuery); ok {
			if !validTxStatus(r.TxStatus) {
				t.Errorf("the recovering Sync carried %q", r.TxStatus)
			}
			return
		}
	}
	t.Fatal("the recovering Sync never produced a readiness")
}
