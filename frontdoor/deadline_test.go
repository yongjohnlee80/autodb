package frontdoor

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// The deadline set (matrix §9).
//
// Every phase before authentication has its OWN budget rather than a share of
// one, so a peer cannot spend the whole allowance on the handshake and still
// be owed time for the startup packet. After authentication the budget
// changes character entirely — from "finish this exchange" to "you may be
// idle" — and getting that transition wrong is invisible until someone is
// halfway through debugging something else.

// deadlineListener starts a listener whose phase budgets are short enough to
// observe, keeping the enforcement path real.
func deadlineListener(t *testing.T, dl deadlines) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, AuthFailuresPerIP: unthrottled,
		testDeadlines: &dl,
	})
	return events, addr
}

// The defaults are the matrix's numbers. A cheap pin, because everything else
// in this file deliberately runs with shortened budgets and would not notice
// an edit to the real ones.
func TestDeadlines_DefaultsMatchTheMatrix(t *testing.T) {
	t.Parallel()
	got := defaultDeadlines()
	want := deadlines{tls: 10 * time.Second, startup: 10 * time.Second, auth: 10 * time.Second, idle: 30 * time.Minute}
	if got != want {
		t.Errorf("defaults = %+v, want %+v (§9: TLS/startup/auth 10s; between-messages idle 30m)", got, want)
	}
}

// A peer that dribbles the S0 request is closed, and gives its slot back.
//
// The second half is the one that matters. Sixty-four connections that each
// hold a pre-auth slot forever are sixty-four connections that close the
// front door to everyone, at a cost to the attacker of one byte every few
// seconds — and a deadline that closes the socket without releasing the
// reservation would leave the door shut anyway.
func TestDeadline_SlowlorisInS0IsClosedAndReleasesItsSlot(t *testing.T) {
	t.Parallel()
	_, events, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, AuthFailuresPerIP: unthrottled,
		MaxConns: 1, PreAuthMaxConns: 1,
		testDeadlines: &deadlines{tls: 250 * time.Millisecond, startup: time.Second, auth: time.Second, idle: time.Minute},
	})

	slow, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = slow.Close() }()
	// One byte of an eight-byte SSLRequest, then nothing.
	if _, werr := slow.Write(sslRequest()[:1]); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	_ = slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, rerr := io.ReadAll(slow); rerr != nil {
		t.Fatalf("the dribbling connection was not closed: %v", rerr)
	}

	// The slot is back: a well-behaved client is admitted where a moment ago
	// the only slot was held.
	waitFor(t, "the slot to be released", func() bool {
		c, derr := net.DialTimeout("tcp", addr, time.Second)
		if derr != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		if _, werr := c.Write(sslRequest()); werr != nil {
			return false
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		answer := make([]byte, 1)
		n, rerr := c.Read(answer)
		return rerr == nil && n == 1 && answer[0] == 'S'
	})
	if _, ok := find(events(), "fd.conn_close"); !ok {
		t.Error("no fd.conn_close for the dribbling connection")
	}
}

// A peer that completes TLS and then dribbles the startup packet is closed on
// the STARTUP budget, not on leftover time from the handshake one.
func TestDeadline_SlowlorisInStartupIsClosed(t *testing.T) {
	t.Parallel()
	_, addr := deadlineListener(t, deadlines{
		tls: 5 * time.Second, startup: 250 * time.Millisecond, auth: 5 * time.Second, idle: time.Minute,
	})

	tc := tlsDial(t, addr)
	packet := startupPacket(protocolVersion30, defaultParams())
	// The length, then one byte of a body that never arrives — the shape a
	// partial-frame stall takes.
	if _, err := tc.Write(packet[:5]); err != nil {
		t.Fatalf("write: %v", err)
	}
	started := time.Now()
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("the stalled startup was not closed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the stalled startup was held for %s against a 250ms budget", elapsed)
	}
}

// A peer that reaches the password prompt and then says nothing is closed on
// the AUTH budget.
//
// This is the cheapest slowloris of all — the server has already spent a TLS
// handshake and a startup parse — so it is the one the auth phase's own
// deadline exists for.
func TestDeadline_SilenceAtThePasswordPromptIsClosed(t *testing.T) {
	t.Parallel()
	_, addr := deadlineListener(t, deadlines{
		tls: 5 * time.Second, startup: 5 * time.Second, auth: 250 * time.Millisecond, idle: time.Minute,
	})

	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	started := time.Now()
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("the silent client was not closed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the silent client was held for %s against a 250ms budget", elapsed)
	}
}

// A frame that declares a body and then stalls is bounded by the same budget.
//
// The declared length is charged before the body is read, so the stall costs
// the server the header and nothing more — but the SOCKET still has to be
// reclaimed, and that is the deadline's job rather than the budget's.
func TestDeadline_APartialFrameDoesNotHoldTheConnection(t *testing.T) {
	t.Parallel()
	_, addr := deadlineListener(t, deadlines{
		tls: 5 * time.Second, startup: 250 * time.Millisecond, auth: 5 * time.Second, idle: time.Minute,
	})

	tc := tlsDial(t, addr)
	// A legal, in-bounds declared length, and then four bytes of it.
	var head [8]byte
	binary.BigEndian.PutUint32(head[0:4], 4+1024)
	binary.BigEndian.PutUint32(head[4:8], protocolVersion30)
	if _, err := tc.Write(head[:]); err != nil {
		t.Fatalf("write: %v", err)
	}
	started := time.Now()
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("the partial frame was not closed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("a half-sent frame held the connection for %s", elapsed)
	}
}

// An authenticated session survives a debug breakpoint.
//
// THIS IS THE CELL THE RE-ARM EXISTS FOR. Every phase before authentication
// runs on a ten-second budget, and a deadline set on a net.Conn STAYS SET —
// so a session that opens and is not explicitly given the idle budget dies
// ten seconds later. It would not look like a deadline: it would look like a
// flaky network, to a developer paused on a breakpoint, and it would be
// reproducible only by pausing for longer than anyone pauses on purpose.
//
// Ninety-five seconds of real time, because the property is about real
// duration and a shortened budget would prove only that a shortened budget
// works. It runs in parallel with the rest of the package, and -short skips
// it for anyone iterating.
func TestDeadline_AnAuthenticatedSessionSurvivesADebugBreakpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("95 seconds of real idling; -short skips it")
	}
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	// The REAL defaults: the pre-auth budgets that would kill this session
	// are the ten-second ones, and the idle budget that saves it is the real
	// thirty-minute one.
	_, _, addr := listenerWith(t, Options{Authn: f, AuthFailuresPerIP: unthrottled})

	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	// The breakpoint. Longer than the 90-second idle-in-transaction bound
	// and nine times the pre-auth budget, which is the number that would
	// have killed it.
	time.Sleep(95 * time.Second)

	// The session is still there and still speaking the protocol.
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	fe.Send(&pgproto3.Query{String: "select 1"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("the session was gone after the pause: %v", err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("the session did not answer after a 95s pause — the pre-auth deadline was "+
			"never replaced with the idle one: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T; this build answers a Query with the not-implemented refusal", msg)
	}
	if e.Detail != "frontdoor/post-auth-not-implemented" {
		t.Errorf("detail = %q, want the honest not-implemented identity", e.Detail)
	}
}

// The post-auth refusal is ACCURATE, not a silence.
//
// A client that sends a Query to a build without the execution slice deserves
// to be told the feature is not there. Dropping the connection instead looks
// like a network fault and sends someone debugging the wrong layer.
func TestSession_AQueryBeforeF1IsRefusedAccurately(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	_, fe := authenticated(t, addr)
	fe.Send(&pgproto3.Query{String: "select 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T, want an ErrorResponse", msg)
	}
	if e.Code != "0A000" {
		t.Errorf("code = %q, want 0A000 feature_not_supported — the accurate SQLSTATE for a "+
			"feature that is not built rather than a permission that was refused", e.Code)
	}
	if e.Detail != "frontdoor/post-auth-not-implemented" {
		t.Errorf("detail = %q; §1.2 puts the front door's own rule id here", e.Detail)
	}
}

// Terminate ends the session cleanly and releases what it held.
func TestSession_TerminateReleasesTheReservation(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	_, fe := authenticated(t, addr)
	fe.Send(&pgproto3.Terminate{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the release", func() bool {
		_, closed := f.calls()
		return len(closed) == 1
	})
}

// authenticated drives a connection all the way to ReadyForQuery('I').
func authenticated(t *testing.T, addr string) (*tls.Conn, *pgproto3.Frontend) {
	t.Helper()
	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return conn, fe
		}
	}
}
