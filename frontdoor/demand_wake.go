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
// connection belongs to the loop that is sitting in Receive on it, and a second
// goroutine writing a fatal frame would interleave its bytes into whatever the
// loop is in the middle of. The result would be a protocol stream no client can
// parse, produced by the code path whose entire purpose is to explain itself
// clearly.
//
// So the engine only ever CLAIMS a victim and knocks. This file is the knock:
// a mailbox the loop alone reads, and a wake that makes the loop's blocking
// read return so it can read it. Everything the client sees is written by the
// loop, in its own time, between frames.

// demandMailbox is one session's terminal notice, and the wake that delivers it.
//
// BUFFERED BY ONE AND NEVER BLOCKING. The engine posts while holding nothing it
// can afford to hold: if the loop is busy, a post that blocked would stall the
// scheduler behind a client that may be halfway through a slow query. One
// notice is all there can ever be, because the claim behind it is single-use.
type demandMailbox struct {
	notices chan exec.DemandNotice
	conn    net.Conn
	now     func() time.Time
}

func newDemandMailbox(conn net.Conn, now func() time.Time) *demandMailbox {
	return &demandMailbox{notices: make(chan exec.DemandNotice, 1), conn: conn, now: now}
}

// post is called by the engine. It must not block and must not write.
func (m *demandMailbox) post(n exec.DemandNotice) {
	select {
	case m.notices <- n:
	default:
		// Already holding one. The claim is single-use, so a second notice is
		// a duplicate rather than new information.
		return
	}
	// THE WAKE IS A READ DEADLINE IN THE PAST, which makes the loop's blocked
	// Receive return a timeout immediately.
	//
	// READ ONLY, not both directions: the loop is about to WRITE a fatal frame,
	// and a write deadline in the past would fail that write and turn an
	// explained ending into a silent disconnection -- the exact outcome this
	// path exists to avoid.
	_ = m.conn.SetReadDeadline(m.now().Add(-time.Second))
}

// take returns a notice the engine posted, if any. It never blocks: by the time
// the loop asks, the notice is either there or it is not.
func (m *demandMailbox) take() (exec.DemandNotice, bool) {
	select {
	case n := <-m.notices:
		return n, true
	default:
		return exec.DemandNotice{}, false
	}
}

// demandReclaimer is the engine capability this path needs.
//
// ASSERTED RATHER THAN REQUIRED, because a listener can be driven by an
// executor that does not schedule leases at all -- several of this package's own
// harnesses are. An executor without it simply never has sessions selected for
// reclamation, which is the safe direction: nothing is ended that cannot be
// explained.
type demandReclaimer interface {
	RegisterDemandWake(id exec.SessionID, f func(exec.DemandNotice))
	FinishDemandReclaim(ctx context.Context, id exec.SessionID, gen uint64) bool
}

// armDemandWake registers this session's mailbox with the engine, and reports
// whether the engine can drive one.
func (l *Listener) armDemandWake(sess exec.WireSessionResult, m *demandMailbox) (demandReclaimer, bool) {
	dr, ok := l.queries.(demandReclaimer)
	if !ok {
		return nil, false
	}
	dr.RegisterDemandWake(sess.SessionID, m.post)
	return dr, true
}

// endForDemand tells the client why its session is ending, and only then lets
// the engine take the lease back.
//
// THE ORDER IS THE CONTRACT, not a preference. Releasing first would let the
// freed lease reach a new session -- whose first statement could reach the
// target -- while this client still believes it holds a connection and has been
// given no reason to think otherwise. The frame goes out, the flush is bounded
// so a client that has stopped reading cannot hold the lease open forever, and
// the release happens after.
//
// IF THE CLIENT CANNOT BE TOLD, THE SESSION STILL ENDS. A flush that fails
// means the connection is already gone or unresponsive; holding the lease for
// it would punish everybody waiting in order to protect a client that is not
// listening. The ending is recorded either way, so the trail never claims a
// frame was delivered that was not.
func (l *Listener) endForDemand(ctx context.Context, conn net.Conn, be *pgproto3.Backend,
	sess exec.WireSessionResult, dr demandReclaimer, n exec.DemandNotice,
	peer string, closeReason *string) error {

	// The wake left a read deadline in the past. Clear it before writing: the
	// frame below is a write, and leaving a stale deadline on the connection
	// would be read by anything that looks at it as a live budget.
	_ = conn.SetReadDeadline(time.Time{})

	row, ok := heldObjectRowFor(condNoMechanism)
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

	// ONE RECORD, WRITTEN HERE, whether or not the client heard it. The idle
	// time is the justification for ending somebody's session and belongs in
	// the record beside the decision.
	l.onEvent(Event{Kind: "fd.session_ended", Reason: row.identity, Peer: peer,
		Detail: demandDetail(n, delivered)})

	// Only now. See the comment above.
	dr.FinishDemandReclaim(ctx, sess.SessionID, n.Gen)
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
