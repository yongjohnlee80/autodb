package frontdoor

import (
	"net"
	"sync/atomic"
)

// acceptToken is what Accept hands on: three obligations as ONE thing.
//
// THE HANDLER REGISTRATION IS THE ONE THAT MATTERS, and an earlier design left
// it out. The accept loop performs the WaitGroup add BEFORE admission and
// before the goroutine exists -- that ordering is the guarantee that Close
// cannot observe the counter at zero while a connection is still arriving --
// and the obligation is then TRANSFERRED into the handler goroutine. It is
// discharged either on the refusal path or by the handler, never both.
//
// A token dropped without discharge is a Close that hangs forever. A token
// discharged twice is a counter that goes negative, which panics the process
// on a shutdown path. Neither has a symptom until the day somebody restarts
// the daemon under load, so the invariant is carried by the type rather than
// by everyone remembering.
//
// EXACTLY ONE CONSUMER TAKES IT. The accept loop either refuses -- and
// discharges here -- or spawns the handler, which discharges in its defer.
//
//	 [Incoming TCP Connection]
//	            │
//	            ▼
//	   ┌─────────────────┐
//	   │ Accept Loop     │ ──► Add to Listener.wg (Handler Registration)
//	   │                 │ ──► Check admission & acquire capacity Ticket
//	   └────────┬────────┘
//	            │
//	   ┌────────┴────────┐
//	   │   acceptToken   │ ──► Linear ownership container
//	   └────────┬────────┘
//	            │
//	     Admitted?
//	     ┌──────┴──────┐
//	    NO            YES
//	     │             │
//	     ▼             ▼
//	[Discharge]  [Transfer Token to Connection Handler]
//	(Immediate)        │
//	                   ▼
//	             [Session Loop Runs]
//	                   │
//	                   ▼
//	             [Handler Defer: Discharge LIFO]
//	             1. untrack()     (Remove from active list)
//	             2. conn.Close()  (Close socket)
//	             3. announce()    (Emit audit event)
//	             4. tkt.release() (Return capacity ticket)
//	             5. handlerDone() (Decrement WaitGroup)
type acceptToken struct {
	conn net.Conn
	tkt  *ticket

	// untrack removes the connection from the listener's live set. Nil until
	// the handler tracks it: the accept loop does not track, so a refusal has
	// nothing to untrack and must not invent one.
	untrack func()

	// announce records the connection's ending. It sits BETWEEN the socket
	// close and the return of the accept-time reservation, which is where the
	// trail has it today and where it has to stay: emitting it after the
	// handler registration is discharged would let Close return -- and the
	// process exit -- with the last event of a connection's life unwritten.
	announce func()

	// handlerDone discharges the WaitGroup add the accept loop performed.
	handlerDone func()

	// lc is the per-connection lifecycle, CREATED IN THE ACCEPT LOOP and
	// carried here with the rest of the obligation.
	//
	// It travels with the token because the accept phase is the first phase:
	// a lifecycle created later could not record that accept ran, and a phase
	// that cannot be recorded cannot be enforced exactly-once either.
	lc *lifecycle

	spent atomic.Bool
}

// track records the connection in the live set and extends the token to own
// its removal.
//
// It happens in the HANDLER, not the accept loop, because the live set exists
// so Close can end connections that are being served -- and one that has not
// reached a handler yet is ended by the accept loop refusing it.
func (t *acceptToken) track(l *Listener) {
	l.track(t.conn)
	t.untrack = func() { l.untrack(t.conn) }
}

// discharge releases everything the token owns, exactly once, in reverse order
// of acquisition.
//
// LIFO IS NOT DECORATION. The tracking entry must go before the socket closes,
// or a concurrent Close walks the live set and closes a connection this
// goroutine is already closing. The ticket must go before the handler
// registration, or Close can observe the counter at zero while the accept-time
// reservation is still held -- the daemon would then tear down the engine
// underneath a connection that still counts against the budget.
func (t *acceptToken) discharge() {
	if !t.spent.CompareAndSwap(false, true) {
		// ALREADY SPENT. Returning quietly is right: the double-discharge
		// this guards is a bug in the caller, and the failure it would
		// otherwise produce -- a negative WaitGroup counter -- panics the
		// process during shutdown, which is the worst possible moment to
		// learn about it.
		return
	}
	if t.untrack != nil {
		t.untrack()
	}
	if t.conn != nil {
		_ = t.conn.Close()
	}
	if t.announce != nil {
		t.announce()
	}
	t.tkt.release()
	if t.handlerDone != nil {
		t.handlerDone()
	}
}

// discharged reports whether the token has been spent, for the cells that
// prove every terminal path spends it.
func (t *acceptToken) discharged() bool { return t.spent.Load() }
