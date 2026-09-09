package exec

// THE SYNC DRIVE'S OWN CONTRACT, asserted at the drive rather than through the
// wire.
//
// Review asked for this directly, and the reason is sound: an end-to-end cell
// can be satisfied by a coincidence of message text, while the drive's RETURN
// TYPE and status byte are the mechanism the loop depends on. If
// WireSyncSegment stops returning an *EmitStopped, the loop's errors.As finds
// nothing and silently falls back to a second, later snapshot of the
// transaction status — which is the split the whole path exists to prevent.
//
// DELIVERY-SCOPED, and the statement fields stay zero. A Sync owns segment
// delivery and no statement: the segment may carry none at all
// (Parse/Describe/Sync, what pgx's default mode and database/sql's Prepare
// send) or several, each already settled by its own Execute drive. A
// statement-scoped report from this drive could only be a claim it cannot
// support — and a first version made exactly that claim by passing
// segmentAwaitsWire as Executed.

import (
	"context"
	"errors"
	"testing"
)

func TestExtPG_SyncReturnsADeliveryScopedReportWithTheAuthoritativeStatus(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	// One target-backed step, so the segment genuinely has something to
	// deliver. No Execute: this is the shape only the Sync drive delivers.
	extPrepare(t, f, sid, userID, "p1", "SELECT 1")

	refuse := errors.New("the consumer stopped reading")
	status, err := f.eng.WireSyncSegment(ctx, sid, userID, func(WireMessage) error {
		return refuse
	})

	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("WireSyncSegment returned %T (%v), not an *EmitStopped — the loop's "+
			"errors.As then finds nothing and falls back to a second snapshot", err, err)
	}
	// The consumer's own error survives the wrap, so errors.Is against a
	// caller's sentinel keeps working.
	if !errors.Is(err, refuse) {
		t.Error("the consumer's cause did not survive the report")
	}
	if !stopped.Delivery {
		t.Error("the report is statement-scoped; a Sync owns delivery and no statement")
	}
	if got := stopped.Arm(); got != ArmDeliveryStopped {
		t.Errorf("Arm() = %q, want %q", got, ArmDeliveryStopped)
	}
	// THE STATUS IS VALID, which is the whole reason only Sync may arm.
	// reportOutputWithheld treats a report with an invalid status as
	// session-lost, so an arm carrying 0 would drop a recoverable session.
	if !validWireTxStatus(stopped.TxStatus) {
		t.Errorf("the report carries status %q, which is invalid", stopped.TxStatus)
	}
	if stopped.TxStatus != status {
		t.Errorf("the report carries %q and the drive returned %q: one snapshot means "+
			"one byte", stopped.TxStatus, status)
	}
	// Nothing downstream can read a statement fact out of it.
	if stopped.Executed || stopped.Outcome != "" || stopped.TargetErr != nil {
		t.Errorf("a delivery report carries statement fields: Executed=%v Outcome=%q "+
			"TargetErr=%v", stopped.Executed, stopped.Outcome, stopped.TargetErr)
	}
}

// validWireTxStatus is the three bytes ReadyForQuery defines, which is the
// same predicate frontdoor.validTxStatus applies to a report's status before
// forwarding it. Duplicated rather than shared because that one is unexported
// in another package; if the two ever disagree, this cell is the one that is
// wrong.
func validWireTxStatus(b byte) bool {
	return b == TxStatusIdle || b == TxStatusInTx || b == TxStatusAborted
}

// THE BOOKKEEPING SURVIVES THE CUT — PART 1: IDLE PORTALS ARE STILL DROPPED.
//
// Review asked for the two bookkeeping fixes to be celled independently of the
// arm, and this is the first. The delivery-failure return used to come BEFORE
// the segment's end-of-life work, so a consumer that stopped reading skipped
// it: the segment count stayed, the wrap stayed open, and — the one a later
// frame can actually observe — the portals stayed in our object store after the
// backend had destroyed them. The next Execute then named a portal that exists
// here and nowhere else.
//
// matrix §4a is the rule: portals do not survive the transaction, prepared
// statements do. 'I' means the target reports none open.
//
// MOVING THE ARM BACK ABOVE THE BOOKKEEPING REDDENS THIS.
func TestExtPG_ADeliveryCutAtSyncStillDropsIdlePortals(t *testing.T) {
	f, _, sid, userID := extSession(t)
	ctx := context.Background()

	// extPrepare is Parse and Bind, so the segment holds a portal and nothing
	// has been executed.
	extPrepare(t, f, sid, userID, "bk", "SELECT 1")
	// POSITIVE CONTROL: the portal is really there to be dropped, so a green
	// below is the drop and not an absence.
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, ok := s.ext.portals["bk"]; !ok {
		t.Fatal("positive control: no portal was created, so this cell cannot observe one surviving")
	}

	status, err := f.eng.WireSyncSegment(ctx, sid, userID, func(WireMessage) error {
		return errors.New("the consumer stopped reading")
	})
	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the cut was not reported as a delivery stop: %v — this cell's premise", err)
	}
	if status != TxStatusIdle {
		t.Fatalf("status = %q, want %q: the drop is conditional on idleness, so a "+
			"different byte means this cell is measuring nothing", status, TxStatusIdle)
	}

	if _, ok := s.ext.portals["bk"]; ok {
		t.Error("the portal survived a cut Sync at 'I': the backend has destroyed it and a " +
			"later Execute would name something that exists only in our store")
	}
	// The wire-visible half of the same fact.
	if err := f.eng.WireExecutePortal(ctx, sid, userID, "bk", 0, testIP, discardEmit); !errors.Is(err, ErrUnknownPortal) {
		t.Errorf("Execute of the cut segment's portal = %v, want ErrUnknownPortal", err)
	}
	// And the prepared statement DOES survive. That is the other half of the
	// object-lifetime rule -- portals die with the transaction, prepared
	// statements outlive it -- and without asserting it this cell would pass
	// just as well if the cut had wiped everything the segment created.
	if err := f.eng.WireBind(ctx, sid, userID, "bk2", "bk", nil, nil, nil); err != nil {
		t.Errorf("re-Bind of the prepared statement after the cut = %v; prepared statements "+
			"outlive the transaction", err)
	}
}

// THE BOOKKEEPING SURVIVES THE CUT — PART 2: THE ARM CARRIES THE CLIENT'S
// STATUS, NOT THE TARGET'S.
//
// A reader outside a client transaction runs inside a hidden READ ONLY
// transaction autodb opened, so the TARGET reports T at Sync and that T is
// OURS. The returned byte is already corrected for this. The ARM is a second
// place the same byte escapes, and reportOutputWithheld takes a non-nil
// report's TxStatus as its ONLY snapshot — so a 'T' in the arm reaches the
// client's effects clause and tells a session with no transaction that its
// statements are pending inside one. A driver acts on that: it sends the COMMIT
// it believes it owes, against a transaction rolled back before the byte
// arrived.
//
// REPLACING `status` WITH `targetStatus` IN THE ARM REDDENS THIS, and nothing
// else in the suite does — the pre-existing wrap cell asserts the RETURNED
// byte, which that mutation leaves correct.
func TestExtPG_ADeliveryCutUnderTheHiddenWrapArmsTheClientsStatus(t *testing.T) {
	f, _, sid, userID, table, _ := readerWireSession(t)
	ctx := context.Background()

	if err := f.eng.WireParse(ctx, sid, userID, "w", "SELECT count(*) FROM "+table, nil, testIP); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// POSITIVE CONTROL: the wrap is open, so the T this cell forbids is
	// genuinely available to leak into the arm.
	s, lerr := f.eng.sessions.lookup(sid, userID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if s.ext == nil || s.ext.roWrap == nil {
		t.Fatal("positive control: no hidden wrap was open, so there is no target T to leak")
	}

	status, err := f.eng.WireSyncSegment(ctx, sid, userID, func(WireMessage) error {
		return errors.New("the consumer stopped reading")
	})
	var stopped *EmitStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the cut was not reported as a delivery stop: %v — this cell's premise", err)
	}
	if stopped.TxStatus == TxStatusInTx {
		t.Error("the arm carries T from the hidden wrap: the client owns no transaction, and " +
			"its effects clause would claim the segment is pending inside one")
	}
	if stopped.TxStatus != TxStatusIdle {
		t.Errorf("the arm carries %q, want %q — the reader has no client transaction",
			stopped.TxStatus, TxStatusIdle)
	}
	if stopped.TxStatus != status {
		t.Errorf("the arm carries %q and the drive returned %q: one snapshot means one byte",
			stopped.TxStatus, status)
	}
}
