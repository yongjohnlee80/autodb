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

// reclaim publishes a notice and knocks, exactly as the scheduler would.
func (d *demandEngine) reclaim(idle time.Duration) bool {
	d.mu.Lock()
	if !d.offering {
		d.mu.Unlock()
		return false
	}
	d.pending = &exec.DemandNotice{Gen: 1, ID: "sess-abc123", IdleFor: idle}
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

	// AND THEN THE CONNECTION ENDS. A terminal frame followed by a session that
	// carries on would be worse than no frame at all.
	if _, err := fe.Receive(); err == nil {
		t.Error("the connection stayed open after a FATAL frame")
	}
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
