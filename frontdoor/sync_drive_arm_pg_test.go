package frontdoor

// F2 ITEM 6: THE SYNC DRIVE ARMS, AND WHAT IT ARMS IS TRUE.
//
// The item asked for the Flush/Sync drives to arm an exec.EmitStopped with
// drive->wire and drive->audit witnesses. Review ruled the symmetric
// requirement wrong and it is: Flush has no terminal byte and must stay
// recoverable. Only Sync can arm truthfully.
//
// WHY ARMING NAIVELY IS WORSE THAN NOT ARMING. EmitStopped.TxStatus is "the
// same byte the loop's readiness would carry", and reportOutputWithheld takes a
// non-nil report's status as its ONLY snapshot: an invalid one means the phase
// is unknown, matrix §6.3 forbids inventing a readiness, and the loop closes. A
// Flush-shaped arm carries 0 -- a Flush does not end the segment, so there is
// no ReadyForQuery for it to carry -- and 0 is not valid. Arming Flush would
// drop a session the client could still recover by Syncing.
//
// AND WHY THE SCOPE MATTERS MORE THAN THE ARM. A first version passed
// segmentAwaitsWire as EmitStopped.Executed. Those are different contracts:
// segmentAwaitsWire means "some step's answer must come from the target",
// Executed means "a statement was dispatched". Parse/Describe/Sync satisfies
// the first and not the second, so the arm reported a statement for a segment
// that had none -- and the vocabulary it borrowed was already false there
// before any of this work: the client was told
//
//   "the statement ran and its outcome is not known to the front door
//    ... read the table to find out"
//
// for a segment carrying no statement, sending an operator to inspect a table
// for effects that never existed. Review named the coercion; the fix is the
// SCOPE, and it corrects the pre-existing message as a consequence.

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// countingQueries is the engine seam with the ONE call this cell is about
// counted. Everything else delegates, so the path under test is the real one.
type countingQueries struct {
	QueryExecutor
	txStatusReads atomic.Int64
}

func (c *countingQueries) WireTxStatus(id exec.SessionID, userID int64) (byte, error) {
	c.txStatusReads.Add(1)
	return c.QueryExecutor.WireTxStatus(id, userID)
}

// THE PRIMARY WITNESS: drive -> wire and drive -> audit, in the delivery
// vocabulary, on a segment with NO EXECUTE.
//
// Parse/Describe/Sync is what pgx's default exec mode and database/sql's
// Prepare send, and it is the shape whose answers only the Sync drive
// delivers: the Execute drive stops at its own terminal, so a segment carrying
// no Execute has no other drive to deliver anything.
//
// DELETING THE ARM REDDENS THIS. Without it reportOutputWithheld falls to its
// separate WireTxStatus read, armFromWhatIsKnown has no report to prefer, and
// the client gets the statement vocabulary again -- which is what the
// assertions below refuse.
func TestPGSyncDrive_ADeliveryStopSaysNothingAboutAStatement(t *testing.T) {
	_, secret, database, eng := pgLoopWithEngine(t)

	cap := int64(64)
	q := &countingQueries{QueryExecutor: eng}
	_, events, listenAddr := listenerWith(t, Options{
		Authn: eng, Queries: q, AuthFailuresPerIP: unthrottled, testOutputCap: &cap,
	})

	fe := pgClient(t, listenAddr, secret, database)
	fe.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1 AS one, 2 AS two, 3 AS three"})
	fe.Send(&pgproto3.Describe{ObjectType: 'S', Name: "s1"})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	var (
		gate    *pgproto3.ErrorResponse
		readies int
		ready   *pgproto3.ReadyForQuery
	)
	for range 32 {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the segment's answer: %v — the front door closed without "+
				"telling the client anything", err)
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
	// PREMISE, FATAL RATHER THAN SKIPPED. An earlier version skipped when the
	// cap failed to bite, which meant this regression evidence could vanish
	// silently the day the sizing drifted. If the premise stops holding, the
	// cell is broken and must say so.
	if gate == nil {
		t.Fatalf("the output cap did not withhold this segment (cap=%d): the cell's own "+
			"premise failed, so there is no arm to inspect. Re-size the cap rather than "+
			"letting this pass", cap)
	}
	if readies != 1 {
		t.Fatalf("readiness frames = %d, want exactly 1 (Sync's own)", readies)
	}
	if !validTxStatus(ready.TxStatus) {
		t.Errorf("readiness carried %q, which is not a valid status", ready.TxStatus)
	}

	// THE CLIENT IS TOLD ABOUT DELIVERY, AND NOT ABOUT A STATEMENT.
	if strings.Contains(gate.Message, "the statement ran") ||
		strings.Contains(gate.Message, "the statement executed") {
		t.Errorf("the client was told about a STATEMENT for a segment that carried "+
			"none: %q", gate.Message)
	}
	if strings.Contains(gate.Hint, "read the table") {
		t.Errorf("the hint sends an operator to read a table for effects that never "+
			"existed: %q", gate.Hint)
	}
	if !strings.Contains(gate.Message, "were not delivered") {
		t.Errorf("the client was not told its answers went undelivered: %q", gate.Message)
	}

	// DRIVE -> AUDIT, IN THE SAME VOCABULARY AND UNDER ITS OWN EVENT KIND.
	//
	// The first version of this cell asserted fd.stmt_outcome, and review
	// caught it: the client's text had stopped inventing a statement and the
	// EVENT KIND still did. That is worse where it lands -- an operator's
	// tooling counts kinds rather than reading sentences, so a dashboard
	// totalling statement outcomes counted a segment that ran nothing.
	//
	// Both halves are asserted. One kind being right while the other is still
	// emitted would leave the miscount exactly as it was.
	var detail string
	for _, e := range events() {
		if e.Kind == eventDeliveryStopped && e.Reason == ruleOutputCap {
			detail = e.Detail
		}
	}
	if detail == "" {
		t.Fatalf("no %s audited under %s.\nevents=%v", eventDeliveryStopped, ruleOutputCap, kinds(events()))
	}
	if !strings.Contains(detail, "delivery=stopped") {
		t.Errorf("the audit recorded %q, which does not say the delivery stopped", detail)
	}
	// AND NO STATEMENT OUTCOME, for a segment that carried no statement. This
	// is the assertion review asked for, and it is the one that reddens if the
	// event is emitted under the old kind as well as the new one.
	if strings.Contains(detail, "effects=") {
		t.Errorf("the delivery event carries an effects= token: %q — that field is read as "+
			"the answer to what happened to a statement's effects, and this event knows "+
			"of none", detail)
	}
	for _, e := range events() {
		if e.Kind == eventStmtOutcome {
			t.Errorf("a statement outcome was audited for a segment that dispatched no "+
				"statement: %s reason=%q detail=%q", e.Kind, e.Reason, e.Detail)
		}
	}

	// THE LOOP SEAM: A REPORT PREVENTS THE FALLBACK READ.
	//
	// reportOutputWithheld takes ONE snapshot. With a report it must use the
	// report's status; the separate WireTxStatus read exists only for paths
	// that have none. If the arm is deleted the loop consults it instead, and
	// this count goes up — which is the structural discrimination review asked
	// for, independent of any message text.
	if n := q.txStatusReads.Load(); n != 0 {
		t.Errorf("the loop read WireTxStatus %d time(s) although the drive supplied a "+
			"report; two snapshots is how the effects clause and the readiness byte "+
			"come to disagree", n)
	}
}

// AND THE FLUSH DRIVE DELIBERATELY DOES NOT ARM.
//
// Pinned as a decision, per review's ruling. A Flush does not end the segment,
// so there is no readiness byte for an arm to carry; the only available value
// is 0, which reportOutputWithheld treats as session-lost. A consumer that
// stopped reading mid-segment has NOT lost its session — it can still Sync —
// so arming here would destroy a recoverable connection to report a stop.
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
		t.Fatalf("the cap did not withhold this Flush's output (cap=%d): the cell's "+
			"premise failed and the recovery it exists to show was never exercised", cap)
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
