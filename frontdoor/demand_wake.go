package frontdoor

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE CLIENT'S SOCKET HAS EXACTLY ONE WRITER, AND IT IS THE SESSION LOOP.
//
// The engine decides WHICH idle session gives up its lease when somebody is
// waiting for one. It cannot tell that session's client, because the client's
// connection belongs to the loop sitting in Receive on it, and a second
// goroutine writing a fatal frame would interleave its bytes into whatever the
// loop is in the middle of — producing a stream no client can parse, from the
// code path whose entire purpose is to explain itself clearly.
//
// So the engine reserves and knocks; this file is the knock and the answer.
//
// THE OFFER LIVES IN THE ENGINE, NOT HERE, AND THAT IS THE SECOND VERSION OF
// THIS FILE. The first kept the receive window in a mailbox on this side while
// the engine kept a flag on its side, and the two could disagree: the scheduler
// could read the flag as open, reserve a session for termination, and only then
// find the window closed because the read had already returned. It then ended a
// session that had in fact won the race, with no way to tell its client —
// repairing an ordinary race by silently terminating somebody. Now the offer,
// the reservation and the notice are one piece of state under one mutex, and
// this file holds only what genuinely belongs to the connection: the knock, and
// the frame.

// knockFor builds the wake for one connection.
//
// A READ DEADLINE IN THE PAST, which makes the loop's blocked Receive return
// immediately. READ ONLY, not both directions: the loop is about to WRITE a
// fatal frame, and a write deadline in the past would fail that write and turn
// an explained ending into a silent disconnection — the exact outcome this path
// exists to avoid.
func knockFor(conn net.Conn, now func() time.Time) func() {
	return func() {
		if err := conn.SetReadDeadline(now().Add(-time.Second)); err != nil {
			// THE KNOCK FAILED, SO THE DOOR COMES OFF ITS HINGES. The session
			// is already reserved for termination; if the owner never wakes,
			// that reservation is inert and its lease is held forever by a
			// session serving nobody. Closing the transport guarantees the read
			// returns. The client will not get its explanation, and the record
			// will say so.
			_ = conn.Close()
		}
	}
}

// demandReclaimer is the engine capability this path needs.
//
// ASSERTED RATHER THAN REQUIRED, because a listener can be driven by an
// executor that does not schedule leases at all — several of this package's own
// harnesses are. An executor without it simply never has sessions selected for
// reclamation, which is the safe direction.
type demandReclaimer interface {
	RegisterDemandWake(id exec.SessionID, knock func())
	OfferReceive(id exec.SessionID) uint64
	RetireReceive(id exec.SessionID, token uint64) (exec.DemandNotice, bool)
	FinishDemandReclaim(ctx context.Context, id exec.SessionID, gen uint64) bool
}

// demandOwner brackets one session's blocking read.
type demandOwner struct {
	dr    demandReclaimer
	id    exec.SessionID
	token uint64
	live  bool
}

// armDemandWake registers this connection's knock with the engine.
func (l *Listener) armDemandWake(sess exec.WireSessionResult, conn net.Conn) *demandOwner {
	o := &demandOwner{id: sess.SessionID}
	dr, ok := l.queries.(demandReclaimer)
	if !ok {
		return o
	}
	o.dr, o.live = dr, true
	dr.RegisterDemandWake(sess.SessionID, knockFor(conn, l.now))
	return o
}

// offer opens the receive window, immediately before the loop blocks reading.
func (o *demandOwner) offer() {
	if o.live {
		o.token = o.dr.OfferReceive(o.id)
	}
}

// retire closes it and returns any notice published inside it.
func (o *demandOwner) retire() (exec.DemandNotice, bool) {
	if !o.live {
		return exec.DemandNotice{}, false
	}
	n, ok := o.dr.RetireReceive(o.id, o.token)
	o.token = 0
	return n, ok
}

// endForDemand tells the client why its session is ending, and only then lets
// the engine take the lease back.
//
// THE ORDER IS THE CONTRACT, not a preference. Releasing first would let the
// freed lease reach a new session — whose first statement could reach the
// target — while this client still believes it holds a connection and has been
// given no reason to think otherwise. The frame goes out, the flush is bounded
// so a client that has stopped reading cannot hold the lease open forever, and
// the release happens after.
//
// EXACTLY ONE OCCURRENCE IS WRITTEN, AND ONLY BY THE CALLER THAT ACTUALLY OWNED
// THE TEARDOWN. Finalisation reports whether this reservation performed the
// ending; if something else got there first, this path stays silent rather than
// recording a second decision about one session.
func (l *Listener) endForDemand(ctx context.Context, conn net.Conn, be *pgproto3.Backend,
	o *demandOwner, n exec.DemandNotice, peer string, closeReason *string) error {

	// The knock left a read deadline in the past. Clear it before writing: the
	// frame below is a write, and a stale deadline on the connection would be
	// read by anything that looks at it as a live budget.
	_ = conn.SetReadDeadline(time.Time{})

	row, ok := heldObjectRowFor(condDemandReclaimed)
	if !ok {
		// UNREACHABLE while the register carries the row, and it fails loudly
		// rather than ending a session with no explanation, which is the one
		// outcome this whole path exists to prevent.
		l.onEvent(Event{Kind: "fd.internal", Reason: OutcomeInternalError, Peer: peer,
			Detail: "demand reclamation has no registered terminal row to render"})
		*closeReason = "internal"
		return nil
	}

	be.Send(gateError(row.severity, row.sqlState, row.message, row.identity, row.hint))
	delivered := l.flushBounded(conn, be) == nil

	// Finalise FIRST, then record — the record is only ours to write if we are
	// the path that actually ended it.
	if o.dr.FinishDemandReclaim(ctx, n.ID, n.Gen) {
		l.onEvent(Event{Kind: "fd.session_ended", Reason: row.identity, Peer: peer,
			Detail: demandDetail(n, delivered)})
	}
	*closeReason = "demand-reclaimed"
	return nil
}

func demandDetail(n exec.DemandNotice, delivered bool) string {
	told := "the client was told"
	if !delivered {
		told = "the client could not be told: the connection was already gone"
	}
	return "session ended so its connection could serve a waiting request; it had been idle " +
		n.IdleFor.Round(time.Second).String() + "; " + told
}
