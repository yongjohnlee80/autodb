package frontdoor

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// WHAT THESE CELLS ARE FOR: everything else about demand reclamation is proved
// at a seam — the predicate, the reservation, the notice. These drive the real
// session loop over a real pipe, because the loop is where the pieces have to
// agree: it is what blocks in Receive, what the knock interrupts, and what must
// choose between a client's frame and a terminal notice that arrived in the
// same instant. A design that is right at every seam can still be wired up
// wrongly, and only a driven loop can tell.

// demandEngine is a QueryExecutor that also schedules leases, so the loop's
// demand path is live. Every decision is the cell's to make: it says when an
// offer is answered and what the notice is.
type demandEngine struct {
	*fakeQueries

	mu       sync.Mutex
	knock    func()
	token    uint64
	offering bool
	pending  *exec.DemandNotice

	offered  chan uint64 // one send per offer, so a cell can wait for the loop to block
	finished chan bool   // one send per finalisation, carrying `delivered`
	finishes int

	// The loop's fate, so a cell that is waiting on a frame can say why one
	// never came instead of timing out.
	loopDone   chan struct{}
	loopReason *string
	loopErr    *error
}

func newDemandEngine() *demandEngine {
	return &demandEngine{
		fakeQueries: okQueries(),
		offered:     make(chan uint64, 64),
		finished:    make(chan bool, 8),
	}
}

func (d *demandEngine) RegisterDemandWake(_ exec.SessionID, knock func()) {
	d.mu.Lock()
	d.knock = knock
	d.mu.Unlock()
}

func (d *demandEngine) OfferReceive(_ exec.SessionID) uint64 {
	d.mu.Lock()
	if d.knock == nil {
		d.mu.Unlock()
		return 0 // no knock, no offer — the engine's own rule
	}
	d.token++
	d.offering = true
	t := d.token
	d.mu.Unlock()
	select {
	case d.offered <- t:
	default:
	}
	return t
}

func (d *demandEngine) RetireReceive(_ exec.SessionID, token uint64) (exec.DemandNotice, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.offering || token == 0 || token != d.token {
		return exec.DemandNotice{}, false
	}
	d.offering = false
	n := d.pending
	d.pending = nil
	if n == nil {
		return exec.DemandNotice{}, false
	}
	return *n, true
}

func (d *demandEngine) FinishDemandReclaim(_ context.Context, _ exec.SessionID, _ uint64, delivered bool) bool {
	d.mu.Lock()
	d.finishes++
	first := d.finishes == 1
	d.mu.Unlock()
	if first {
		d.finished <- delivered
	}
	return first
}

// reclaimHolding is reclaim for a victim that still had objects on its backend.
func (d *demandEngine) reclaimHolding(idle time.Duration) bool {
	return d.reclaimAs(idle, true)
}

// reclaim publishes a notice and knocks, exactly as the scheduler would.
func (d *demandEngine) reclaim(idle time.Duration) bool { return d.reclaimAs(idle, false) }

func (d *demandEngine) reclaimAs(idle time.Duration, held bool) bool {
	d.mu.Lock()
	if !d.offering {
		d.mu.Unlock()
		return false
	}
	d.pending = &exec.DemandNotice{Gen: 1, ID: "sess-abc123", IdleFor: idle, HeldObjects: held}
	knock := d.knock
	d.mu.Unlock()
	knock()
	return true
}

func (d *demandEngine) finishCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.finishes
}

// drivenSession runs the real session loop over a pipe and gives the cell the
// client end of it.
//
// THE LOOP'S EXIT IS WATCHED, NOT IGNORED. The first version started runSession
// in a goroutine and dropped its return value, so when the loop ended early the
// cell simply blocked on a frame that was never coming and died at the test
// timeout — a hang where a diagnosis should have been. A hang says only "one of
// these five cells is wrong"; the close reason says which decision the loop
// took and why. Every wait below fails with that reason instead of waiting out
// the clock.
func drivenSession(t *testing.T, d *demandEngine) (*pgproto3.Frontend, *demandEngine, func() []Event, func() string) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	l, events, _ := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: d, AuthFailuresPerIP: unthrottled,
	})

	fr := newFrameReader(server)
	be := pgproto3.NewBackend(fr, server)
	var (
		closeReason string
		loopErr     error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// CLOSED WHEN THE LOOP ENDS, because that is what the real caller does.
		// Without it the client's end of the pipe never sees EOF, so a cell
		// checking that the connection ends waits forever on a close the
		// harness -- not the product -- failed to perform.
		defer func() { _ = server.Close() }()
		loopErr = l.runSession(context.Background(), server, fr, be, goodSession(), "127.0.0.1:5", &closeReason)
	}()
	d.loopDone, d.loopReason, d.loopErr = done, &closeReason, &loopErr

	return pgproto3.NewFrontend(client, client), d, events, func() string {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the session loop never returned")
		}
		return closeReason
	}
}

// receiveOrExplain reads one frame, or fails with the reason the loop ended
// rather than blocking until the test times out.
func receiveOrExplain(t *testing.T, fe *pgproto3.Frontend, d *demandEngine) pgproto3.BackendMessage {
	t.Helper()
	type result struct {
		msg pgproto3.BackendMessage
		err error
	}
	got := make(chan result, 1)
	go func() {
		m, err := fe.Receive()
		got <- result{m, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the client got no frame, only a broken connection: %v%s", r.err, d.whyLoopEnded())
		}
		return r.msg
	case <-time.After(5 * time.Second):
		t.Fatalf("no frame arrived within five seconds%s", d.whyLoopEnded())
		return nil
	}
}

// expectConnectionEnds requires the client's next read to fail, within a bound.
//
// BOUNDED LIKE EVERY OTHER WAIT HERE. A bare Receive on a connection that stays
// open blocks until the whole test binary is killed, and a package-wide timeout
// panic discards the buffered output of every cell that had already recorded
// something -- so one uninstrumented wait can destroy the evidence from all the
// others. That is exactly what it did.
func expectConnectionEnds(t *testing.T, fe *pgproto3.Frontend, d *demandEngine) {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		_, err := fe.Receive()
		ended <- err
	}()
	select {
	case err := <-ended:
		if err == nil {
			t.Error("the connection stayed open after a FATAL frame; a terminal frame " +
				"followed by a session that carries on is worse than no frame at all")
		}
	case <-time.After(5 * time.Second):
		t.Errorf("the connection did not end after the FATAL frame%s", d.whyLoopEnded())
	}
}

// whyLoopEnded reports how the session loop finished, if it has.
func (d *demandEngine) whyLoopEnded() string {
	select {
	case <-d.loopDone:
		return fmt.Sprintf(" — the session loop had already ended: closeReason=%q err=%v. "+
			"It stopped before writing anything, so no frame was ever coming.",
			*d.loopReason, *d.loopErr)
	default:
		return " — the session loop is still running, so it is blocked rather than finished."
	}
}

// awaitOffer waits for the loop to be genuinely blocked reading.
func awaitOffer(t *testing.T, d *demandEngine) {
	t.Helper()
	select {
	case <-d.offered:
	case <-time.After(5 * time.Second):
		t.Fatal("the session loop never offered to receive, so it was never blocked reading " +
			"and nothing could have been delivered to it" + d.whyLoopEnded())
	}
}

// AN IDLE CLIENT IS WOKEN AND SEES THE EXACT FATAL BEFORE THE CONNECTION ENDS.
//
// This is the whole promise of the path: a developer whose session is taken
// finds out why, in a frame, rather than discovering it as a dropped
// connection.
func TestDrivenDemand_AnIdleClientIsToldBeforeTheConnectionEnds(t *testing.T) {
	fe, d, events, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)

	if !d.reclaim(90 * time.Minute) {
		t.Fatal("the loop was not offering when the reclamation was attempted")
	}

	msg := receiveOrExplain(t, fe, d)
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("the client received %T, want the terminal ErrorResponse", msg)
	}
	if errResp.Severity != "FATAL" {
		t.Errorf("severity = %q, want FATAL: the session is over", errResp.Severity)
	}
	if errResp.Code != sqlStateAdminShutdown {
		t.Errorf("SQLSTATE = %q, want %q", errResp.Code, sqlStateAdminShutdown)
	}
	for _, wrong := range []string{"prepared statement", "portal"} {
		if strings.Contains(strings.ToLower(errResp.Message), wrong) {
			t.Errorf("the message claims the client held a %s, which selection never required", wrong)
		}
	}
	if !strings.Contains(strings.ToLower(errResp.Hint), "reconnect") {
		t.Errorf("hint = %q, want it to name the client's only remedy", errResp.Hint)
	}

	// AND THEN THE CONNECTION ENDS.
	expectConnectionEnds(t, fe, d)
	if reason := wait(); reason == "" {
		t.Error("the loop recorded no close reason")
	}
	if n := d.finishCount(); n != 1 {
		t.Errorf("finalisation ran %d times, want exactly 1 — one ending is one release", n)
	}
	for _, e := range events() {
		if e.Kind == "fd.internal" {
			t.Errorf("the loop reported an internal fault: %s", e.Detail)
		}
	}
}

// THE LEASE IS RELEASED PROMPTLY, NOT AFTER A QUIESCE BOUND.
//
// THIS CELL GUARDS A REMOVED SELF-JOIN. Finalisation once claimed the teardown
// slot before calling the close, whose own quiesce claims that same slot — so
// it waited on a channel only its own deferred release would close, and every
// successful reclamation sat out the full bound before releasing the lease it
// had just freed for somebody who was waiting. The whole point of the path,
// undone by its own bookkeeping, and invisible except as latency.
func TestDrivenDemand_TheReleaseHappensPromptlyAfterTheFlush(t *testing.T) {
	fe, d, _, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)

	// A client that reads, so the flush completes rather than being bounded out.
	go func() {
		for {
			if _, err := fe.Receive(); err != nil {
				return
			}
		}
	}()

	start := time.Now()
	d.reclaim(time.Hour)
	select {
	case delivered := <-d.finished:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("the lease was released %s after the frame — a reclamation that takes "+
				"this long is waiting on something, and the request it was freeing "+
				"capacity for is still waiting too", elapsed)
		}
		if !delivered {
			t.Error("the record says the client was not told, though it was reading")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lease was never released")
	}
	wait()
}

// A CLIENT FRAME THAT WINS THE RACE IS SERVED, AND NOTHING IS RECLAIMED.
//
// The client was talking; there is no reclamation to record and no session to
// end. A path that reclaimed anyway would terminate somebody who had just
// proved they were using the connection.
func TestDrivenDemand_AFrameThatWinsIsServedAndNothingIsReclaimed(t *testing.T) {
	fe, d, _, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)

	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}

	sawRows := false
	for range 8 {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		if _, ok := msg.(*pgproto3.DataRow); ok {
			sawRows = true
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Severity == "FATAL" {
			t.Fatalf("the client was terminated although its own frame won the race: %s", e.Message)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if !sawRows {
		t.Error("the client's query was not served")
	}
	if n := d.finishCount(); n != 0 {
		t.Errorf("finalisation ran %d times for a session nobody reclaimed, want 0", n)
	}

	fe.Send(&pgproto3.Terminate{})
	_ = fe.Flush()
	wait()
}

// AN ORDINARY IDLE CLIENT IS NOT MISTAKEN FOR A RECLAMATION.
//
// A client that simply stops talking and one that has been selected both bring
// the loop back from its read. Only a notice tells them apart, and treating a
// silent client as a reclamation would send it a terminal frame blaming
// something that never happened.
func TestDrivenDemand_AnOrdinaryDisconnectIsNotAReclamation(t *testing.T) {
	fe, d, events, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)

	// The client goes away without anything having been reclaimed.
	fe.Send(&pgproto3.Terminate{})
	_ = fe.Flush()

	reason := wait()
	if n := d.finishCount(); n != 0 {
		t.Errorf("finalisation ran %d times for a client that simply left, want 0", n)
	}
	if strings.Contains(reason, "demand") {
		t.Errorf("close reason = %q, which records a reclamation that never happened", reason)
	}
	for _, e := range events() {
		if e.Reason == OutcomeDemandReclaimed {
			t.Error("an ordinary disconnect was recorded as a demand reclamation")
		}
	}
}

// A KNOCK ARRIVING WITH NO OFFER OPEN IS REFUSED RATHER THAN STRANDING ANYONE.
//
// The scheduler must learn that this session is not reclaimable, so it reserves
// a different one instead of committing against a window that has closed.
func TestDrivenDemand_AReclamationIsRefusedWhenNoOfferIsOpen(t *testing.T) {
	fe, d, _, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)

	// Close the window the way the loop does when its read returns.
	if _, ok := d.RetireReceive("sess-abc123", d.token); ok {
		t.Fatal("a notice was delivered when none had been published")
	}
	if d.reclaim(time.Hour) {
		t.Error("a reclamation was accepted with no offer open; it would commit against a " +
			"window that has closed and strand the lease")
	}
	if n := d.finishCount(); n != 0 {
		t.Errorf("finalisation ran %d times, want 0", n)
	}

	fe.Send(&pgproto3.Terminate{})
	_ = fe.Flush()
	wait()
}

// A HOLDER OF OBJECTS GETS THE SAME FRAME, WORD FOR WORD.
//
// THIS CELL EXISTS BECAUSE THE MESSAGE HAS TO BE TRUE OF BOTH. An earlier
// version of this path reused a row telling the client it held prepared
// statements or portals, which was false for most of the sessions it ended.
// The correction was a message that says only what is true of every holder --
// and the way that correction rots is for the two populations to drift apart,
// one of them quietly acquiring a different, more specific frame that is wrong
// again for the other.
//
// Both kinds terminate: the scheduled unit is the wire lease, held for the
// session's lifetime, so detaching a backend frees nothing for anyone waiting.
// The difference between them belongs in the record, not on the wire.
func TestDrivenDemand_AHolderOfObjectsGetsTheSameFrame(t *testing.T) {
	frameFor := func(t *testing.T, holding bool) *pgproto3.ErrorResponse {
		t.Helper()
		fe, d, _, wait := drivenSession(t, newDemandEngine())
		awaitOffer(t, d)
		ok := d.reclaim(time.Hour)
		if holding {
			ok = true
		}
		if !ok {
			t.Fatal("the loop was not offering when the reclamation was attempted")
		}
		msg := receiveOrExplain(t, fe, d)
		got, isErr := msg.(*pgproto3.ErrorResponse)
		if !isErr {
			t.Fatalf("the client received %T, want the terminal ErrorResponse", msg)
		}
		expectConnectionEnds(t, fe, d)
		wait()
		return got
	}

	clean := frameFor(t, false)

	fe, d, _, wait := drivenSession(t, newDemandEngine())
	awaitOffer(t, d)
	if !d.reclaimHolding(time.Hour) {
		t.Fatal("the loop was not offering when the reclamation was attempted")
	}
	msg := receiveOrExplain(t, fe, d)
	holding, isErr := msg.(*pgproto3.ErrorResponse)
	if !isErr {
		t.Fatalf("a holder of objects received %T, want the terminal ErrorResponse", msg)
	}
	expectConnectionEnds(t, fe, d)
	wait()

	if holding.Severity != clean.Severity || holding.Code != clean.Code {
		t.Errorf("a holder of objects got %s/%s and a clean session got %s/%s; both are "+
			"terminated for the same reason and are owed the same answer",
			holding.Severity, holding.Code, clean.Severity, clean.Code)
	}
	if holding.Message != clean.Message {
		t.Errorf("the two populations drifted apart:\n  holder: %q\n  clean:  %q\n"+
			"a message specific to one of them is wrong for the other, which is the "+
			"defect this row was rewritten to fix", holding.Message, clean.Message)
	}
	for _, wrong := range []string{"prepared statement", "portal"} {
		if strings.Contains(strings.ToLower(holding.Message), wrong) {
			t.Errorf("the message names a %s; selection never required one, so it is false "+
				"for every holder with an empty object store", wrong)
		}
	}
}
