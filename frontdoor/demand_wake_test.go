package frontdoor

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/outcome"
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
		// FATAL, BECAUSE THE REST OF THIS CELL INDEXES readSet. It used to be
		// an Errorf, so a knock that set no read deadline reached readSet[0]
		// and PANICKED -- which the mutation runner scores INVALID, not red.
		// The cell had already said the right thing and then destroyed its own
		// verdict two lines later.
		t.Fatalf("the knock set %d read deadlines, want 1 — a blocked Receive is not interrupted", read)
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
	row := demandTerminalRow()
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

// RECLAMATION IS NOT IN THE REGISTER OF REFUSALS, AND PUTTING IT BACK MUST HURT.
//
// THIS IS THE CELL THAT KEEPS THE BOUNDARY. The row lived in the held-object
// register once, because frameHeldObject was a convenient way to send a fatal
// frame -- and it inherited a contract about what it MEANT. Every row there is
// a client asking for something and being told no, raised by an engine
// sentinel. A termination we chose is neither. Left there, either an operator
// counting refusals counts terminations too, or an invariant that holds for
// every genuine member gets loosened to admit one that is not.
func TestDemandReclaimed_ItsOutcomeIsNotAHeldObjectCondition(t *testing.T) {
	for _, row := range heldObjectRegister() {
		if row.identity == OutcomeDemandReclaimed {
			t.Error("demand reclamation is back in the held-object register; it is not a " +
				"condition a client's statements or portals produced, and it is not a refusal")
		}
	}
	for _, row := range heldObjectReserved() {
		if row.identity == OutcomeDemandReclaimed {
			t.Error("demand reclamation is in the held-object RESERVED table, which is for " +
				"object conditions awaiting a producer; it has a producer and is not one")
		}
	}

	// Its own producer declares it, exactly once, as a Control.
	var found int
	for _, reg := range Outcomes() {
		if reg.Producer != ProducerDemandReclamation {
			continue
		}
		for _, d := range reg.Outcomes {
			if string(d.ID) != OutcomeDemandReclaimed {
				continue
			}
			found++
			if d.Kind != outcome.Control {
				t.Errorf("declared %s, want Control: nobody was refused, a session was ended", d.Kind)
			}
			if d.Charge != outcome.NotApplicable {
				t.Errorf("charged %s, want NotApplicable: the session is long past every "+
					"accept-time budget, so no per-source counter is in reach", d.Charge)
			}
		}
	}
	if found != 1 {
		t.Errorf("its producer declares the outcome %d times, want exactly 1", found)
	}
}
