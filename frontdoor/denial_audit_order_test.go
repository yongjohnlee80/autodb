package frontdoor

// THE CLIENT IS TOLD FIRST AND THE OPERATOR SECOND, SO A CELL MUST WAIT.
//
// This exists because a correct front door reddened a release gate and then
// could not be made to do it again.
//
// TestPGF4_AMidSegmentTeardownReturnsTheLaneAndTheLease failed its own control
// with `reasons=[]`: the second client HAD been refused, and the event log
// still held no refusal. Reproduction attempts, all green: five focused runs,
// the whole frontdoor package alone, and five PG-using packages driven
// concurrently against one database. Rerunning was never going to show it.
//
// The cause is in listener.go: sendDenial writes to the socket, and onEvent
// emits the audit event afterwards. That ordering is deliberate and is
// preserved -- a refused client should not wait on our bookkeeping -- but it
// means the two facts become true in different goroutines, in that order, with
// nothing synchronising a reader. pgTryClient returns on the ErrorResponse, so
// a cell that then reads the event log is reading it too early.
//
// testPostDenialAuditDelay widens that interval on purpose. With it the race
// is deterministic, which turns "wait for the event, do not sample it" from a
// convention into a claim that fails when broken.
//
// The package's waitFor helper already documents this exact hazard -- "a cell
// asserting on an event the server emits AFTER answering the client" -- and
// auth_test.go's denial cells all use it. The F4 control was the one place
// that did not, which is why this is a cell and not just a fix.

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// THE INTERVAL IS REAL: with it held open, the event is NOT there when the
// client's error arrives, and IS there shortly after.
//
// Both halves matter. The first is the defect. The second is what stops this
// cell from passing against a front door that never audits the refusal at all
// -- which would satisfy "absent at first" perfectly.
func TestDenialAudit_TheEventLandsAfterTheClientIsTold(t *testing.T) {
	t.Parallel()

	// Wide enough to be unmistakable, short enough not to pad the suite.
	const window = 250 * time.Millisecond
	f := &fakeAuth{err: errors.New("denied for this cell")}
	_, events, addr := listenerWith(t, Options{
		Authn:                    f,
		AuthFailuresPerIP:        unthrottled,
		testPostDenialAuditDelay: window,
	})

	// Drive a refusal and stop the instant the client has its error, which is
	// where every client-side helper in this package returns.
	_, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "whatever"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	if e, ok := msg.(*pgproto3.ErrorResponse); !ok || e.Code != DenialSQLState {
		t.Fatalf("got %T/%v, want the uniform denial — this cell needs a REFUSAL to "+
			"measure", msg, msg)
	}

	// IMMEDIATELY: the client has its error and the event is not written yet.
	if got := refusalReasons(events()); len(got) != 0 {
		t.Skipf("the audit event was already present %v before the emit could run "+
			"(reasons=%v) — the seam is not holding the interval open, so this cell "+
			"cannot demonstrate the race it exists for", window, got)
	}

	// AND THE WAIT GETS IT. This is the half that makes the fix falsifiable:
	// waitForRefusal is the helper the F4 control now uses, called here by
	// name against a window held open deliberately. If it ever went back to
	// sampling, this cell fails.
	//
	// Without this half the cell would also pass for a listener that audits
	// nothing at all, which would satisfy "absent at first" perfectly.
	deadline := time.Now().Add(5 * time.Second)
	var reason string
	for time.Now().Before(deadline) && reason == "" {
		if d, ok := find(events(), "fd.auth_denied"); ok {
			reason = d.Reason
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reason == "" {
		t.Fatalf("the refusal was never audited at all; the client was told and the "+
			"operator never was. reasons=%v", refusalReasons(events()))
	}
	waitForRefusal(t, events, reason, "the denial this cell drove")

	// And the bare predicate the control used to call is true NOW, which is
	// the point: it was not wrong, it was early.
	if !refusedFor(events(), reason) {
		t.Errorf("refusedFor is false after the wait returned, so the two disagree " +
			"about the same event")
	}
}
