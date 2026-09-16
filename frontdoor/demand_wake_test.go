package frontdoor

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// deadlineConn records which deadlines were set on it, and whether it was
// closed. The knock's whole job is to end a blocked read, so which deadline it
// touches IS the behaviour under test.
type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	readSet  []time.Time
	writeSet []time.Time
	bothSet  []time.Time
	closed   int
	readErr  error
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readSet = append(c.readSet, t)
	return c.readErr
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

func (c *deadlineConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *deadlineConn) counts() (read, write, both, closed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.readSet), len(c.writeSet), len(c.bothSet), c.closed
}

// THE KNOCK ENDS THE READ AND LEAVES THE WRITE ALONE.
//
// THIS IS THE CELL THAT PROTECTS THE EXPLANATION. The loop is woken precisely
// so that it can WRITE a fatal frame saying why the session is ending. A
// deadline in the past applied to both directions — the obvious thing to reach
// for, and what the loop's other budgets use — would fail that write, and a
// developer whose session was taken would get a silent disconnection instead of
// the sentence explaining it. That is the outcome this entire path exists to
// prevent, and it would look like a one-word difference in a diff.
func TestDemandKnock_TouchesTheReadDeadlineOnly(t *testing.T) {
	now := time.Now()
	conn := &deadlineConn{}

	knockFor(conn, func() time.Time { return now })()

	read, write, both, closed := conn.counts()
	if read != 1 {
		t.Errorf("the knock set %d read deadlines, want 1 — a blocked Receive is not interrupted", read)
	}
	if write != 0 || both != 0 {
		t.Errorf("the knock set %d write and %d combined deadlines, want 0 of each — a write "+
			"deadline in the past fails the fatal frame, and the client is disconnected "+
			"with no explanation", write, both)
	}
	if closed != 0 {
		t.Error("a successful knock closed the transport, so the frame can never be written")
	}
	conn.mu.Lock()
	armed := conn.readSet[0]
	conn.mu.Unlock()
	if !armed.Before(now) {
		t.Errorf("the read deadline was set to %s, which is not in the past, so a read that "+
			"is already blocked will not return", armed)
	}
}

// A KNOCK THAT CANNOT BE DELIVERED TAKES THE DOOR OFF ITS HINGES.
//
// By the time the knock happens the session is ALREADY reserved for
// termination. If the owner never wakes, that reservation is inert: the session
// serves nobody, nothing is coming to finish it, and the lease it was meant to
// free is held forever. Closing the transport guarantees the read returns.
func TestDemandKnock_AFailedWakeClosesTheTransport(t *testing.T) {
	conn := &deadlineConn{readErr: errors.New("connection already torn down")}

	knockFor(conn, time.Now)()

	if _, _, _, closed := conn.counts(); closed != 1 {
		t.Errorf("the transport was closed %d times after the knock failed, want 1 — "+
			"otherwise the reservation is inert and its lease is never released", closed)
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
func TestDemandReclaimed_TheTerminalMessageIsTrueOfEveryHolderItEnds(t *testing.T) {
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
func TestDemandReclaimed_TheNoMechanismRowIsNotClaimed(t *testing.T) {
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
