package frontdoor

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// Matrix row 4a:Session-end — what a mid-segment teardown leaves behind.
//
// WHY THE ROW STOOD OPEN, and why the obvious cell would not have closed it.
// The lane half was already witnessed — TestExtStall_SilentSegmentIsTornDownAnd
// ReleasesTheLane watches general.inUse() come back to zero after a teardown.
// What no cell asserted was the REGISTRY half, and the reason it went unasserted
// is that the argument for it sounds complete: the per-session account dies with
// the session object, so there is nothing left to release. That is an argument
// about lifetime, not a witness. A session object that is garbage but still
// admitted holds its lease exactly as firmly as a live one, and every existing
// cell would pass while it did.
//
// SO THE LEASE IS THE INSTRUMENT. Under a cap of one, "the lease came back" is
// not an accounting claim to be reasoned about — it is something a second client
// either can or cannot do, and the front door answers.
//
// THE CONTROL IS THE POINT OF THE CELL (feedback: check what a cell ACCEPTS).
// Without step 2, this cell passes when the cap is not applied at all: B would
// connect after the teardown because B could always have connected, and the cell
// would report a released lease it never observed. Step 2 is what makes B's
// later success mean something, and it is asserted on the AUDIT REASON, not
// merely on "B was refused" — the pre-auth vocabulary is uniform 28000, so a
// refusal for any other cause is indistinguishable on the wire.
//
// MUTATION-PROVEN on a green baseline, three mutants, each caught at the
// assertion it is aimed at rather than by a control:
//
//   - the front door never calls CloseWireSession        -> step 6 red
//   - the engine never removes the row from the registry -> step 7 red
//   - CloseWireSession is a no-op                        -> step 6 red
//
// THE SECOND ONE IS WHY STEPS 6 AND 7 ARE BOTH HERE, and it is not what I
// expected: with sessions.remove() disabled, step 6 still PASSES. The lookup
// refuses a session in the closing state whether or not its row is gone, so
// "the registry forgot it" and "the lease came back" are two different facts
// and neither implies the other. A cell asserting only the first would report
// a clean teardown while every lease the front door ever took was still spent.
//
// TEARDOWN IS MID-SEGMENT, deliberately: Parse/Bind/Execute and a Flush, with no
// Sync. The segment is open, a portal is suspended, and the reservation is held
// at the instant the socket dies — which is the shape the row names, and the one
// where a leak would survive.
func TestPGF4_AMidSegmentTeardownReturnsTheLaneAndTheLease(t *testing.T) {
	// A cap of one, which is what turns the whole question into an observation.
	l := pgLoopFull(t, exec.WithLeaseCap(1))
	conn, fe := pgClientWithConn(t, l.addr, l.secret, l.database)

	// 1. A GOES MID-SEGMENT. Execute, Flush, no Sync.
	fe.Send(&pgproto3.Parse{Name: "se", Query: "SELECT g FROM generate_series(1,5) g"})
	fe.Send(&pgproto3.Bind{DestinationPortal: "sp", PreparedStatement: "se"})
	fe.Send(&pgproto3.Execute{Portal: "sp", MaxRows: 2})
	// The protocol's Flush, which collects the answers without ending the
	// segment. fe.Flush() below only writes the client's buffer out.
	fe.Send(&pgproto3.Flush{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	held := readUntil(t, conn, fe, untilSuspended)

	// CONTROL: the segment is genuinely open and holding something. Without
	// this the teardown below could be of a session that never started, and
	// "the lease came back" would be a claim about nothing.
	if hasError(held) {
		t.Fatalf("control: the segment errored before the teardown: %v", errorText(held))
	}
	if _, ok := firstOfType[*pgproto3.PortalSuspended](held); !ok {
		t.Fatalf("control: no PortalSuspended, so no portal is live at teardown; frames=%v",
			kindsOf(held))
	}
	for _, m := range held {
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			t.Fatalf("control: readiness arrived, so the segment CLOSED — this cell means to "+
				"tear down mid-segment and did not; frames=%v", kindsOf(held))
		}
	}

	// 2. CONTROL: the registry KNOWS this session while it is alive. Without
	// this, step 5's "no such session" could be a session id that was never
	// admitted, or a userID mismatch, and the cell would report a teardown it
	// never observed.
	sid := openedSession(t, l.events())
	if _, err := l.eng.WireTxStatus(sid, l.patUserID); err != nil {
		t.Fatalf("control: the registry does not know the live session %q: %v", sid, err)
	}

	// 3. CONTROL: the cap binds, and A is the one holding the lease.
	if _, errResp := pgTryClient(t, l.addr, l.secret, l.database); errResp == nil {
		t.Fatal("control: a second client connected while the first held the only lease, so " +
			"the cap is not binding and this cell would report a released lease whatever " +
			"the teardown did")
	}
	if !refusedFor(l.events(), exec.DenyLeaseCap) {
		t.Fatalf("control: the second client was refused, but not for the lease cap — the "+
			"pre-auth vocabulary is uniform, so a refusal for another cause looks identical "+
			"on the wire and would not tell us A holds the lease.\nreasons=%v",
			refusalReasons(l.events()))
	}

	// 4. THE TEARDOWN. The socket dies with the segment open.
	_ = conn.Close()

	// 5. THE LANE COMES BACK.
	waitFor(t, "the lane held by the torn-down segment to be released",
		func() bool { return l.lis.general.inUse() == 0 })

	// 6. THE REGISTRY FORGOT THE SESSION — the half the row was open for, asked
	// of the registry directly rather than argued from object lifetime.
	waitFor(t, "the registry to forget the torn-down session", func() bool {
		_, err := l.eng.WireTxStatus(sid, l.patUserID)
		return errors.Is(err, exec.ErrSessionNotFound)
	})

	// 7. AND THE LEASE IT HELD CAME BACK — the half the row was open for. B is the same
	// attempt that was refused in step 2; the only thing that changed is that A
	// went away mid-segment.
	waitFor(t, "the torn-down session's lease to be returned to the registry", func() bool {
		fe2, errResp := pgTryClient(t, l.addr, l.secret, l.database)
		return errResp == nil && fe2 != nil
	})
}

// untilSuspended stops at the frame that says the portal is live and the segment
// is not finished. untilReady would block here forever: there is no Sync.
func untilSuspended(m pgproto3.BackendMessage) bool {
	switch m.(type) {
	case *pgproto3.PortalSuspended, *pgproto3.CommandComplete, *pgproto3.ErrorResponse:
		return true
	}
	return false
}

// pgTryClient is pgClientWithConn without the fatal: a refusal is an ANSWER
// here, not a broken fixture.
func pgTryClient(t *testing.T, addr, secret, database string) (*pgproto3.Frontend, *pgproto3.ErrorResponse) {
	t.Helper()
	conn, fe := startupTo(t, addr, map[string]string{
		"user": "root", "database": database, "application_name": "psql",
	})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: secret})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the answer to the password: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			return nil, m
		case *pgproto3.ReadyForQuery:
			return fe, nil
		}
	}
}

// A pre-auth refusal is audited as fd.auth_denied — matched on the KIND as well
// as the reason, so a reason string appearing on some other event cannot stand
// in for the denial this control is about.
func refusedFor(evs []Event, reason string) bool {
	for _, e := range evs {
		if e.Kind == "fd.auth_denied" && e.Reason == reason {
			return true
		}
	}
	return false
}

func refusalReasons(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == "fd.auth_denied" || e.Kind == "fd.refused" {
			out = append(out, e.Kind+":"+e.Reason)
		}
	}
	return out
}

// openedSession is the SessionID the listener recorded for the connection it
// just admitted. The client never learns it; the audit trail does.
func openedSession(t *testing.T, evs []Event) exec.SessionID {
	t.Helper()
	var ids []string
	for _, e := range evs {
		if e.Kind == "fd.session_open" {
			ids = append(ids, e.Detail)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("want exactly one opened session in the audit trail, got %d (%v)", len(ids), ids)
	}
	return exec.SessionID(ids[0])
}
