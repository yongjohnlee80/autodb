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
// TWO CELLS, BECAUSE ONE OF THEM CANNOT DO BOTH JOBS.
//
// The first version tried. It drove a real refusal through a widened window,
// then asserted the ordering, then called waitForRefusal "by name" to pin the
// helper. Review measured it and both halves were unsound:
//
//   - The ordering premise was a SLEEP, so "the event is not there yet" was a
//     belief about a window that might already have elapsed. The cell skipped
//     when it did -- which is a green that reports nothing, in the one place
//     the evidence mattered.
//   - The helper witness was not a witness. The cell polled until
//     fd.auth_denied existed and THEN called waitForRefusal, so the event was
//     already present on its first look. Replacing waitForRefusal with a
//     single immediate sample still passed 10/10.
//
// So the ordering is now held by a BARRIER, where the absence is a fact rather
// than a hope and its failure is fatal; and the helper is celled DIRECTLY
// against an event source that is absent on the first call and present after
// it, where an immediate sample cannot pass. Neither premise can skip.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// THE ORDERING IS REAL: the client has its error while the audit event
// provably does not exist yet, and the event lands once the gate opens.
//
// Both halves matter. The first is the defect a sampling reader hits. The
// second is what stops this cell from passing against a front door that never
// audits the refusal at all -- which would satisfy "absent at first"
// perfectly.
func TestDenialAudit_TheEventLandsAfterTheClientIsTold(t *testing.T) {
	t.Parallel()

	// THE BARRIER. While this is open the listener is parked between
	// sendDenial and onEvent, so the emit cannot have run -- which is what
	// makes the absence below an assertion rather than a race.
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }

	f := &fakeAuth{err: errors.New("denied for this cell")}
	_, events, addr := listenerWith(t, Options{
		Authn:                   f,
		AuthFailuresPerIP:       unthrottled,
		testPostDenialAuditGate: gate,
	})
	// RELEASED WHATEVER HAPPENS, and registered AFTER listenerWith on purpose.
	//
	// Cleanups run LIFO, and listenerWith's own cleanup closes the listener and
	// waits on its WaitGroup. Registering this first put the release BEHIND
	// that wait, so any t.Fatal between here and the explicit release below
	// left the listener waiting on a goroutine parked on a gate nobody would
	// close until after the wait returned. That is a deadlock, and it cost the
	// whole package go test's 600s panic timeout when a mutation run tripped
	// it. Registered second, this runs FIRST and unparks the goroutine before
	// anything waits on it.
	t.Cleanup(release)

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

	// THE CLIENT HAS ITS ERROR AND THE EVENT IS NOT WRITTEN. Fatal rather than
	// skipped: the gate is still closed, so an event here means the audit ran
	// BEFORE the socket write and the ordering this cell documents is wrong.
	if got := refusalReasons(events()); len(got) != 0 {
		t.Fatalf("the refusal was audited before the client was told (reasons=%v) — the "+
			"emit is parked on the gate, so this is the ordering changing, not a "+
			"scheduling artefact", got)
	}

	// AND THE EVENT LANDS ONCE THE GATE OPENS. Still a wait, because releasing
	// the barrier only unparks the listener's goroutine; when it gets to run
	// is not ours to say. That is the same asymmetry every denial cell in this
	// package faces, and the reason waitFor exists.
	release()
	waitForRefusal(t, events, string(reasonAuthStoreError), "the denial this cell drove")

	// And the bare predicate the F4 control used to call is true NOW, which is
	// the point: it was not wrong, it was early.
	if !refusedFor(events(), string(reasonAuthStoreError)) {
		t.Errorf("refusedFor is false after the wait returned, so the two disagree " +
			"about the same event")
	}
}

// THE HELPER ITSELF WAITS, celled where a single sample cannot pass.
//
// This is the witness the previous round claimed and did not have. The event
// source is EMPTY on its first call and carries the refusal from the second,
// which is the shape the production race produces: the reader arrives before
// the writer. A waitForRefusal that samples once sees the empty answer and
// fails; one that waits sees the second.
//
// It needs no listener, no socket and no window, so it cannot skip, cannot
// flake, and says nothing about timing — only about the helper's contract.
func TestWaitForRefusal_WaitsForALaterEventRatherThanSamplingOnce(t *testing.T) {
	t.Parallel()

	const reason = "frontdoor/auth-store-error"
	var calls atomic.Int64
	events := func() []Event {
		if calls.Add(1) == 1 {
			// The reader arrived first. This is precisely the state the F4
			// control read and reported as reasons=[].
			return nil
		}
		return []Event{{Kind: "fd.auth_denied", Reason: reason, Peer: "test"}}
	}

	// A sampling implementation fails HERE, inside the helper, with "no
	// refusal was audited under ... reasons=[]" — the same message the
	// original gate failure carried.
	waitForRefusal(t, events, reason, "an event that lands after the first look")

	if n := calls.Load(); n < 2 {
		t.Fatalf("waitForRefusal returned after %d call(s) to the event source: it took "+
			"one sample and happened to be right, which is the behaviour this cell "+
			"exists to forbid", n)
	}
}

// AND IT STILL FAILS WHEN THE EVENT NEVER COMES, so the wait above is not
// simply "keep looking until the deadline and pass".
//
// Asserted through the helper's own t.Fatalf, captured rather than triggered:
// waitForRefusal takes *testing.T, so the only way to observe its failure
// without failing this cell is to hand it a T of its own and run it where a
// Fatal ends that goroutine and not this one.
func TestWaitForRefusal_FailsWhenTheEventNeverArrives(t *testing.T) {
	t.Parallel()

	// A source that is empty forever. The helper's deadline is 5s, so this
	// cell costs that -- which is the price of proving the negative, and it
	// runs in parallel with the rest.
	done := make(chan bool, 1)
	go func() {
		fake := &testing.T{}
		defer func() {
			// A t.FailNow inside a non-test goroutine ends it via runtime.Goexit,
			// so the report has to be written from a deferred call.
			done <- fake.Failed()
		}()
		waitForRefusal(fake, func() []Event { return nil }, "frontdoor/never", "an event that never lands")
	}()
	if !<-done {
		t.Error("waitForRefusal returned successfully against an event source that never " +
			"produced the refusal: the wait would then certify silence")
	}
}
