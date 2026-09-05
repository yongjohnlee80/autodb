package frontdoor

import (
	"errors"
	"testing"

	"crypto/tls"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// withParam copies the default startup params with one field changed, so each
// caller below differs in exactly one way from the others.
func withParam(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base))
	for bk, bv := range base {
		out[bk] = bv
	}
	out[k] = v
	return out
}

// A LOCKED STORE answers 57P03, not the uniform 28000 (ADR-0087 Amendment 1
// A1.3) — and it answers the SAME 57P03 to everybody.
//
// The reviewer asked for this proven with a DECOY rather than a mutation, and
// gave the reason: "somebody gets 57P03" proves nothing about the callers that
// matter. The property is CONSTANCY. A valid token, an invalid token, an
// unknown user and an unknown connection must be byte-identical on the wire
// while the store is locked, because the moment they differ this stops being a
// global server state and becomes the enumeration oracle R13 forbids.
func TestLockedStore_AnswersTheSameToEveryCaller(t *testing.T) {
	t.Parallel()
	// The engine returns ErrLocked regardless of what is presented, which is
	// what a locked store DOES: the lock is hit at DSN decrypt, after the
	// credential is read and before anything about it is known to the caller.
	f := &fakeAuth{err: auth.ErrLocked}
	_, addr := authListener(t, f)

	// FOUR CALLERS that differ in every way a caller can differ.
	callers := []struct {
		name     string
		params   map[string]string
		password string
	}{
		{"a valid-looking token", defaultParams(), "adb_pat_aaaaaaaaaa.bbbbbbbb"},
		{"an invalid token", defaultParams(), "not-a-token-at-all"},
		{"an unknown user", withParam(defaultParams(), "user", "nobody-here"), "adb_pat_aaaaaaaaaa.bbbbbbbb"},
		{"an unknown connection", withParam(defaultParams(), "database", "no-such-connection"), "adb_pat_aaaaaaaaaa.bbbbbbbb"},
	}

	var first *pgproto3.ErrorResponse
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			_, fe := startupTo(t, addr, c.params)
			if _, err := fe.Receive(); err != nil {
				t.Fatal(err)
			}
			fe.Send(&pgproto3.PasswordMessage{Password: c.password})
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
			if e.Code != LockedSQLState {
				t.Fatalf("code = %q, want %q — a developer with a good token is being told "+
					"their credentials are wrong", e.Code, LockedSQLState)
			}
			if e.Message != LockedMessage {
				t.Errorf("message = %q, want the fixed %q", e.Message, LockedMessage)
			}
			// NOTHING THAT VARIES. A HINT or a DETAIL carrying the cause is
			// exactly the disclosure A1.3 refused: the caller learns the
			// STATE, never the reason.
			if e.Hint != "" {
				t.Errorf("the refusal carries a HINT (%q); the cause belongs in the log", e.Hint)
			}
			if first == nil {
				cp := *e
				first = &cp
				return
			}
			// THE DECOY'S POINT: identical to the first caller's, field by
			// field. Anything that differs is an oracle.
			if e.Code != first.Code || e.Message != first.Message ||
				e.Detail != first.Detail || e.Hint != first.Hint ||
				e.Severity != first.Severity {
				t.Errorf("this caller's refusal differs from the first:\n got %+v\nwant %+v", *e, *first)
			}
		})
	}
}

// The locked answer is NOT charged to the peer's throttle — OBSERVED, not
// asserted about the audit row.
//
// The first version of this cell checked only that the audit reason was
// `frontdoor/store-locked`, which is a claim about the LOG rather than about
// the throttle: its name promised more than it observed, and the shared
// `authListener` helper runs `AuthFailuresPerIP: unthrottled`, so it could not
// have seen charging even if it had looked. This drives a listener with a real
// limit and asks the only question that matters — after enough locked
// refusals to exhaust the budget, is the address still admitted?
//
// Charging would lock out exactly the addresses that must connect the moment
// the window opens: the developers waiting for the box to finish coming up.
// The outage is ours.
func TestLockedStore_IsNotChargedToThePeer(t *testing.T) {
	t.Parallel()
	// 10 is the floor the protocol matrix pins for auth_failures_per_ip — the
	// listener refuses to Open below it, so the cell uses the real number
	// rather than a convenient one.
	const limit = 10

	// CLOSES THE CONNECTION. The first version discarded the *tls.Conn, so ten
	// refusals left ten sockets open and a CONNECTION cap tripped before the
	// failure budget did — the cell was measuring the wrong limit and said so
	// by failing inside the TLS negotiation.
	refuse := func(t *testing.T, addr string) *pgproto3.ErrorResponse {
		t.Helper()
		tc, fe := startupTo(t, addr, defaultParams())
		defer tc.Close()
		if _, err := fe.Receive(); err != nil {
			t.Fatal(err)
		}
		fe.Send(&pgproto3.PasswordMessage{Password: "adb_pat_aaaaaaaaaa.bbbbbbbb"})
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
		return e
	}

	// POSITIVE CONTROL FIRST. An ORDINARY credential failure against the same
	// limit MUST throttle — otherwise this harness cannot observe charging at
	// all and the assertion below would pass against any implementation.
	t.Run("control: an ordinary failure IS charged", func(t *testing.T) {
		_, _, addr := listenerWith(t, Options{
			// A REAL DENIAL, not a bare error. The first version of this
			// control used errors.New("no such token"), which the listener
			// correctly treats as OUR failure rather than the peer's — so it
			// is ALSO uncharged, and the control was measuring nothing. The
			// control caught that, which is what a control is for.
			Authn:             &fakeAuth{err: exec.WireDenial(exec.DenyBadCredential)},
			AuthFailuresPerIP: limit,
		})
		// Exactly the budget, and no more: past it the source is refused at
		// ADMISSION and a further refuse() would fail inside the TLS
		// negotiation rather than returning a frame.
		for i := 0; i < limit; i++ {
			if e := refuse(t, addr); e.Code != DenialSQLState {
				t.Fatalf("attempt %d: code = %q, want the uniform denial", i, e.Code)
			}
		}
		if !throttled(t, addr) {
			t.Fatalf("%d ordinary credential DENIALS against a limit of %d did not throttle "+
				"the source; this harness cannot observe charging, so the locked-store "+
				"assertion below would prove nothing", limit, limit)
		}
	})

	// THE PROPERTY. The same number of LOCKED refusals must leave the address
	// admitted.
	_, events, addr := listenerWith(t, Options{
		Authn:             &fakeAuth{err: auth.ErrLocked},
		AuthFailuresPerIP: limit,
	})
	// COMFORTABLY past the budget the control just proved is enforceable. If
	// any of these were charged, a later one would be refused at admission and
	// refuse() would fail rather than return a frame — so the loop completing
	// is itself part of the assertion.
	for i := 0; i < limit+3; i++ {
		e := refuse(t, addr)
		if e.Code != LockedSQLState {
			t.Fatalf("attempt %d: code = %q, want %q", i, e.Code, LockedSQLState)
		}
	}
	if throttled(t, addr) {
		t.Fatal("locked-store refusals were CHARGED to the peer: the addresses that must " +
			"connect the moment the window opens are locked out by our own outage")
	}

	waitFor(t, "the audit row", func() bool { _, ok := find(events(), "fd.auth_denied"); return ok })
	denied, _ := find(events(), "fd.auth_denied")
	if denied.Reason != string(reasonStoreLocked) {
		t.Errorf("audit reason = %q, want %q — the operational trail must say what this was",
			denied.Reason, reasonStoreLocked)
	}
}

// throttled reports whether this source is now refused at ADMISSION.
//
// It dials WITHOUT the shared tlsDial helper on purpose: a throttled source is
// reset during the TLS handshake, and tlsDial calls t.Fatalf on that — so a
// probe built on it can report "not throttled" and can never report
// "throttled", which is the half that matters. Measured: with the budget
// exhausted the connection is reset before the SSL answer arrives.
func throttled(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return true
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(sslRequest()); err != nil {
		return true
	}
	answer := make([]byte, 1)
	if _, err := c.Read(answer); err != nil || answer[0] != 'S' {
		// Reset, or refused the SSL negotiation: the admission refusal.
		return true
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // not the subject
	if err := tc.Handshake(); err != nil {
		return true
	}
	defer tc.Close()
	if _, err := tc.Write(startupPacket(protocolVersion30, defaultParams())); err != nil {
		return true
	}
	fe := pgproto3.NewFrontend(tc, tc)
	msg, err := fe.Receive()
	if err != nil {
		return true
	}
	// An AuthenticationCleartextPassword request means we were ADMITTED.
	if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Code == DenialSQLState {
		return true
	}
	return false
}

// THE DECOY FOR THE EXCEPTION ITSELF: every OTHER refusal still gets the
// uniform 28000.
//
// This is the cell that reddens if the one exception ever becomes a table. A
// map from reason to code is how a uniform surface turns into an enumerable
// one, one well-meaning entry at a time, and the whole argument for A1.3 was
// that a locked store is a GLOBAL state rather than a per-caller answer.
func TestLockedStore_EveryOtherRefusalIsStillUniform(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a store failure", errors.New("meta store: connection refused")},
		{"an ordinary credential failure", errors.New("auth: no such token")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAuth{err: tc.err}
			_, addr := authListener(t, f)
			_, fe := startupTo(t, addr, defaultParams())
			if _, err := fe.Receive(); err != nil {
				t.Fatal(err)
			}
			fe.Send(&pgproto3.PasswordMessage{Password: "whatever"})
			if err := fe.Flush(); err != nil {
				t.Fatal(err)
			}
			msg, err := fe.Receive()
			if err != nil {
				t.Fatal(err)
			}
			e, ok := msg.(*pgproto3.ErrorResponse)
			if !ok {
				t.Fatalf("got %T, want an ErrorResponse", msg)
			}
			if e.Code != DenialSQLState || e.Message != DenialMessage {
				t.Fatalf("code/message = %q/%q, want the uniform %q/%q — the 57P03 exception "+
					"has grown beyond the one state A1.3 argued for",
					e.Code, e.Message, DenialSQLState, DenialMessage)
			}
		})
	}
}
