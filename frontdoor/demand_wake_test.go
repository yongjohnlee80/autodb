package frontdoor

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// deadlineConn records which deadlines were set on it, and nothing else. The
// wake's whole job is to end a blocked read, so which deadline it touches IS
// the behaviour under test.
type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	readSet  []time.Time
	writeSet []time.Time
	bothSet  []time.Time
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readSet = append(c.readSet, t)
	return nil
}

func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeSet = append(c.writeSet, t)
	return nil
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bothSet = append(c.bothSet, t)
	return nil
}

func (c *deadlineConn) counts() (read, write, both int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.readSet), len(c.writeSet), len(c.bothSet)
}

// THE WAKE ENDS THE READ AND LEAVES THE WRITE ALONE.
//
// THIS IS THE CELL THAT PROTECTS THE EXPLANATION. The loop is woken precisely
// so that it can WRITE a fatal frame saying why the session is ending. A
// deadline in the past applied to both directions — the obvious thing to reach
// for, and what the loop's other budgets use — would fail that write, and a
// developer whose session was taken would get a silent disconnection instead of
// the sentence explaining it. That is the outcome this entire path exists to
// prevent, and it would look like a one-word difference in a diff.
func TestDemandWake_TouchesTheReadDeadlineOnly(t *testing.T) {
	now := time.Now()
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, func() time.Time { return now })

	m.offer()
	if !m.post(exec.DemandNotice{Gen: 1, IdleFor: time.Hour}) {
		t.Fatal("a notice posted inside the receive window was refused")
	}

	read, write, both := conn.counts()
	if read != 1 {
		t.Errorf("the wake set %d read deadlines, want 1 — a blocked Receive is not interrupted", read)
	}
	if write != 0 || both != 0 {
		t.Errorf("the wake set %d write and %d combined deadlines, want 0 of each — a write "+
			"deadline in the past fails the fatal frame, and the client is disconnected "+
			"with no explanation", write, both)
	}
	conn.mu.Lock()
	armed := conn.readSet[0]
	conn.mu.Unlock()
	if !armed.Before(now) {
		t.Errorf("the read deadline was set to %s, which is not in the past, so a read that is "+
			"already blocked will not return", armed)
	}
}

// THE NOTICE REACHES THE LOOP, AND ONLY THE LOOP READS IT.
func TestDemandWake_TheNoticeIsDeliveredOnce(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)
	m.offer()
	m.post(exec.DemandNotice{Gen: 7, IdleFor: 90 * time.Minute})

	n, ok := m.retire()
	if !ok {
		t.Fatal("the loop found no notice after one was posted")
	}
	if n.Gen != 7 || n.IdleFor != 90*time.Minute {
		t.Errorf("notice = %+v, want the posted generation and idle time", n)
	}
	if _, again := m.retire(); again {
		t.Error("the same notice was delivered twice — one claim is one ending")
	}
}

// A POST THAT CANNOT BE DELIVERED NEVER BLOCKS THE SCHEDULER.
//
// The engine posts while a request is waiting on it. If a post could block on a
// loop that is busy — mid-query, or slow to come round — one idle client would
// stall admission for everybody.
func TestDemandWake_PostingNeverBlocks(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)

	m.offer()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more posts than the mailbox can hold, with nothing consuming.
		for i := range 100 {
			m.post(exec.DemandNotice{Gen: uint64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("posting a notice blocked — a busy session loop can stall the admission queue")
	}

	if _, ok := m.retire(); !ok {
		t.Error("no notice survived")
	}
	if _, again := m.retire(); again {
		t.Error("more than one notice was queued; the claim behind them is single-use, so a " +
			"second is a duplicate rather than new information")
	}
}

// AN ORDINARY IDLE CLIENT IS NOT MISTAKEN FOR A RECLAIM.
//
// Both arrive at the loop as a read timeout. The mailbox is the only thing that
// tells them apart, so an empty mailbox has to mean "this was the client", or
// every silent client would be answered with a terminal frame blaming a
// reclamation that never happened.
func TestDemandWake_AnEmptyMailboxIsNotAWake(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)
	if _, ok := m.retire(); ok {
		t.Error("the loop read a notice nobody posted, so an ordinary idle timeout would be " +
			"reported to the client as a demand reclamation")
	}
	if read, write, both := conn.counts(); read != 0 || write != 0 || both != 0 {
		t.Errorf("a mailbox that was never posted to touched the connection (%d/%d/%d)", read, write, both)
	}
}

// A NOTICE IS ONLY ACCEPTED WHILE THE OWNER CAN ACT ON IT.
//
// THIS IS THE CELL THAT REPLACED A BROKEN DESIGN. The wake works by putting a
// read deadline in the past, and the loop re-arms its ordinary deadline every
// time round. A notice posted while the loop was BETWEEN reads therefore had
// its knock erased a moment later: it went dormant, the session was never
// reclaimed, and the request waiting for that lease waited its entire bound for
// a reclamation that had already been decided and could never happen.
//
// So the window is open only between the loop arming its read and that read
// returning, and a post outside it is REFUSED rather than silently dropped —
// the scheduler needs to learn this session is not reclaimable so it can
// reserve a different one instead of stranding this one.
func TestDemandWake_ANoticeIsRefusedOutsideTheReceiveWindow(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)

	if m.post(exec.DemandNotice{Gen: 1}) {
		t.Error("a notice was accepted before the owner offered to receive one; its knock " +
			"would be erased by the next re-arm and the lease would be stranded")
	}
	if read, _, _ := conn.counts(); read != 0 {
		t.Error("a refused post still touched the connection")
	}

	m.offer()
	if !m.post(exec.DemandNotice{Gen: 2}) {
		t.Fatal("a notice posted inside the window was refused")
	}

	// The read returns: the window closes with it.
	if _, ok := m.retire(); !ok {
		t.Fatal("the notice was not delivered when the read returned")
	}
	if m.post(exec.DemandNotice{Gen: 3}) {
		t.Error("a notice was accepted after the window closed")
	}
}

// A SECOND NOTICE IS REFUSED WHILE ONE IS ALREADY PENDING.
//
// One reservation is one ending. Accepting a second would let two reservations
// believe they own the same session.
func TestDemandWake_ASecondNoticeIsRefusedWhileOneIsPending(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)
	m.offer()
	if !m.post(exec.DemandNotice{Gen: 1}) {
		t.Fatal("the first notice was refused")
	}
	if m.post(exec.DemandNotice{Gen: 2}) {
		t.Error("a second notice was accepted while one was already pending — two " +
			"reservations would each believe they own this session's ending")
	}
	n, _ := m.retire()
	if n.Gen != 1 {
		t.Errorf("delivered generation %d, want the first", n.Gen)
	}
}

// THE TERMINAL OWNER IS SINGULAR EVEN WHEN THE CLIENT IS TALKING.
//
// Two paths through the loop can now find the same notice — the one after a
// frame and the one after a read ends. Only the first may act on it, or the
// client is framed twice and the lease released twice.
func TestDemandWake_OnlyOnePathCanActOnANotice(t *testing.T) {
	conn := &deadlineConn{}
	m := newDemandMailbox(conn, time.Now)
	m.offer()
	m.post(exec.DemandNotice{Gen: 5})

	first, okFirst := m.retire()
	_, okSecond := m.retire()

	if !okFirst || first.Gen != 5 {
		t.Fatalf("the first path did not receive the notice (got %+v, ok=%v)", first, okFirst)
	}
	if okSecond {
		t.Error("a second path through the loop also received the notice — the client would be " +
			"sent two terminal frames and the lease released twice")
	}
}

// THE TERMINAL MESSAGE SAYS ONLY WHAT IS TRUE OF EVERY SESSION IT ENDS.
//
// THIS CELL EXISTS BECAUSE THE FIRST VERSION SAID SOMETHING FALSE. It reused
// the reserved no-mechanism row, which tells the client it held prepared
// statements or portals. Selection deliberately does not require those — an
// idle holder with an empty object store is reclaimed just the same — so most
// clients ending this way would have been told something untrue about their own
// session, in the one message whose whole job is to explain what happened. A
// developer who had opened no statements would go hunting for statements they
// never created.
func TestDemandWake_TheTerminalMessageIsTrueOfEveryHolderItEnds(t *testing.T) {
	row, ok := heldObjectRowFor(condDemandReclaimed)
	if !ok {
		t.Fatal("demand reclamation has no registered terminal row, so it would end sessions " +
			"with no explanation at all")
	}
	if row.identity != OutcomeDemandReclaimed {
		t.Errorf("identity = %q, want %q — ending a session is not a capacity refusal and an "+
			"operator must be able to count the two separately", row.identity, OutcomeDemandReclaimed)
	}
	if row.severity != "FATAL" {
		t.Errorf("severity = %q, want FATAL: the session is over", row.severity)
	}
	for _, claim := range []string{"prepared statement", "portal"} {
		if strings.Contains(strings.ToLower(row.message), claim) {
			t.Errorf("the message claims the client held a %s, which selection never required, "+
				"so it is false for every holder with an empty object store", claim)
		}
	}
	if !strings.Contains(strings.ToLower(row.hint), "reconnect") {
		t.Errorf("hint = %q, want it to name reconnecting — it is the only remedy the client has", row.hint)
	}
}

// THE NO-MECHANISM ROW STAYS RESERVED UNTIL SOMETHING CAN HONESTLY RAISE IT.
//
// A row in the runtime register claims a path the code has. Nothing classifies
// the object-holding subset yet, so promoting it would make the manifest
// describe an outcome no input can produce.
func TestDemandWake_TheNoMechanismRowIsNotClaimedByDemandReclamation(t *testing.T) {
	for _, row := range heldObjectRegister() {
		if row.condition == condNoMechanism {
			t.Error("the no-mechanism row is registered as producible, but nothing classifies " +
				"the object-holding sessions it describes")
		}
	}
	var reserved bool
	for _, row := range heldObjectReserved() {
		if row.condition == condNoMechanism {
			reserved = true
		}
	}
	if !reserved {
		t.Error("the no-mechanism row is neither registered nor reserved, so its agreed wire " +
			"answer has been lost rather than deferred")
	}
}
