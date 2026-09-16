package frontdoor

import (
	"context"
	"net"
	"sync"
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
// So the engine reserves and offers; this file is the offer.
//
// THE OFFER IS ONLY OPEN WHILE THE LOOP IS ACTUALLY BLOCKED READING, and that
// window is the whole design rather than a detail of it. An earlier version
// registered a callback for the session's whole life, which was not the same
// thing at all: the wake works by putting a read deadline in the past, so a
// notice posted while the loop was BETWEEN reads had its knock erased by the
// loop re-arming its ordinary budget a moment later. The notice went dormant,
// the session was never reclaimed, and the request waiting for that lease
// waited its entire bound for a reclamation that had already been decided and
// could never happen. A window that is open exactly when it can be acted on is
// the only version of this that is honest.

// demandMailbox is one session's terminal notice, its receive epoch, and the
// wake that delivers it.
type demandMailbox struct {
	mu sync.Mutex
	// offering is true only between the loop arming its read and that read
	// returning. A post outside that window is refused rather than dropped
	// silently, so the scheduler learns the session is not reclaimable and can
	// reserve somebody else.
	offering bool
	notice   *exec.DemandNotice

	conn net.Conn
	now  func() time.Time
}

func newDemandMailbox(conn net.Conn, now func() time.Time) *demandMailbox {
	return &demandMailbox{conn: conn, now: now}
}

// offer opens the window. Called immediately before the loop blocks in Receive.
func (m *demandMailbox) offer() {
	m.mu.Lock()
	m.offering = true
	m.mu.Unlock()
}

// retire closes the window and returns any notice that arrived inside it.
//
// CALLED AS THE READ RETURNS, BEFORE THE FRAME OR THE ERROR IS LOOKED AT. Both
// a delivered frame and a wake bring the loop back here, and if the frame were
// handled first the client's statement would run on a session the engine has
// already reserved for termination — two owners for one ending. Retiring first
// makes the winner explicit: a notice that got in before the read returned wins,
// and the frame is not dispatched.
func (m *demandMailbox) retire() (exec.DemandNotice, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.offering = false
	n := m.notice
	m.notice = nil
	if n == nil {
		return exec.DemandNotice{}, false
	}
	return *n, true
}

// post is called by the engine. It must not block and must not write to the
// protocol stream. It reports whether the owner accepted the notice.
//
// A REFUSAL HERE IS INFORMATION, NOT A FAILURE. It means this session is not
// currently reclaimable, and the scheduler needs to know that so it can reserve
// a different one instead of stranding this one.
func (m *demandMailbox) post(n exec.DemandNotice) bool {
	m.mu.Lock()
	if !m.offering || m.notice != nil {
		m.mu.Unlock()
		return false
	}
	m.notice = &n
	m.mu.Unlock()

	// THE WAKE IS A READ DEADLINE IN THE PAST, which makes the loop's blocked
	// Receive return immediately.
	//
	// READ ONLY, not both directions: the loop is about to WRITE a fatal frame,
	// and a write deadline in the past would fail that write and turn an
	// explained ending into a silent disconnection — the exact outcome this
	// path exists to avoid.
	if err := m.conn.SetReadDeadline(m.now().Add(-time.Second)); err != nil {
		// THE KNOCK FAILED, SO THE DOOR IS TAKEN OFF ITS HINGES. The session is
		// already reserved for termination; if the owner never wakes, that
		// reservation is inert and its lease is held forever by a session that
		// serves nobody. Closing the transport guarantees the read returns.
		// The client will not get its explanation, and the record will say so.
		_ = m.conn.Close()
	}
	return true
}

// demandReclaimer is the engine capability this path needs.
//
// ASSERTED RATHER THAN REQUIRED, because a listener can be driven by an
// executor that does not schedule leases at all — several of this package's own
// harnesses are. An executor without it simply never has sessions selected for
// reclamation, which is the safe direction.
type demandReclaimer interface {
	RegisterDemandWake(id exec.SessionID, f func(exec.DemandNotice) bool)
	SetReclaimable(id exec.SessionID, offering bool)
	FinishDemandReclaim(ctx context.Context, id exec.SessionID, gen uint64) bool
}

// demandOwner couples one session's mailbox to the engine that offers to it.
type demandOwner struct {
	mbox *demandMailbox
	dr   demandReclaimer
	id   exec.SessionID
	live bool
}

// armDemandWake registers this session's mailbox with the engine.
func (l *Listener) armDemandWake(sess exec.WireSessionResult, conn net.Conn) *demandOwner {
	o := &demandOwner{mbox: newDemandMailbox(conn, l.now), id: sess.SessionID}
	dr, ok := l.queries.(demandReclaimer)
	if !ok {
		return o
	}
	o.dr, o.live = dr, true
	dr.RegisterDemandWake(sess.SessionID, o.mbox.post)
	return o
}

// offer and retire bracket the loop's blocking read. The engine's view of
// whether this session is reclaimable is kept in step with the window, so a
// session is never SELECTED at a moment when its notice could not be acted on.
func (o *demandOwner) offer() {
	if !o.live {
		return
	}
	o.mbox.offer()
	o.dr.SetReclaimable(o.id, true)
}

func (o *demandOwner) retire() (exec.DemandNotice, bool) {
	if !o.live {
		return exec.DemandNotice{}, false
	}
	o.dr.SetReclaimable(o.id, false)
	return o.mbox.retire()
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

	// The wake left a read deadline in the past. Clear it before writing: the
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

	// Finalise FIRST, then record — because the record is only ours to write if
	// we are the path that actually ended it.
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
