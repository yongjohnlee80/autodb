package frontdoor

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// Rows 2.6-2.9 on a real socket.
//
// The chain that DECIDES is exec's and has its own cells against a live
// PostgreSQL. What is under test here is the wire around it: that the only
// method offered is cleartext, that whatever comes back becomes either the
// protocol's success sequence or the one uniform denial, and that the
// reservation the chain took is released however the connection ends.

// fakeAuth stands in for the engine. An interface exists precisely so this
// can be a struct with two fields rather than a meta store and a live
// database — the engine's own behaviour is not what these cells are about,
// and a cell that needs a database to prove a protocol property tests both
// and localizes neither.
type fakeAuth struct {
	mu sync.Mutex

	result exec.WireSessionResult
	err    error

	// deadlines records the ctx deadline each verification was handed, so a
	// cell can prove the authenticator is bounded by the SAME budget the
	// rest of the credential exchange spent — rather than the no-deadline
	// listener context it used to get.
	deadlines []time.Time
	hasDeadln []bool

	// opened and closed record the calls, so a cell can assert the
	// reservation was released rather than assume it.
	opened []string // the presented credentials, in order
	closed []string // "<session-id>/<reason>"
	// closeCtxErr records the state of the context the release was handed,
	// so a cell can prove the teardown was not given a context that had
	// already been cancelled by the very shutdown that triggered it.
	closeCtxErr []error
}

func (f *fakeAuth) OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (exec.WireSessionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dl, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, dl)
	f.hasDeadln = append(f.hasDeadln, ok)
	f.opened = append(f.opened, presented+"|"+startupUser+"|"+database+"|"+ip)
	if f.err != nil {
		return exec.WireSessionResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeAuth) CloseWireSession(ctx context.Context, id exec.SessionID, _ int64, _, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, string(id)+"/"+reason)
	f.closeCtxErr = append(f.closeCtxErr, ctx.Err())
}

func (f *fakeAuth) calls() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...), append([]string(nil), f.closed...)
}

func (f *fakeAuth) authDeadlines() ([]time.Time, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.deadlines...), append([]bool(nil), f.hasDeadln...)
}

func (f *fakeAuth) closeContexts() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.closeCtxErr...)
}

func goodSession() exec.WireSessionResult {
	return exec.WireSessionResult{
		SessionID: "sess-abc123", UserID: 7, ConnID: 3,
		AdmissionSource: "global", PATName: "laptop", UserName: "Root",
	}
}

// authListener starts a listener with an engine behind it.
func authListener(t *testing.T, f *fakeAuth) (func() []Event, string) {
	t.Helper()
	_, events, addr := listenerWith(t, Options{Authn: f, AuthFailuresPerIP: unthrottled})
	return events, addr
}

// startupTo drives a client through TLS and the StartupMessage, and returns
// the connection sitting where the server's next word is the auth request.
func startupTo(t *testing.T, addr string, params map[string]string) (*tls.Conn, *pgproto3.Frontend) {
	t.Helper()
	tc := tlsDial(t, addr)
	if _, err := tc.Write(startupPacket(protocolVersion30, params)); err != nil {
		t.Fatalf("startup: %v", err)
	}
	return tc, pgproto3.NewFrontend(tc, tc)
}

func defaultParams() map[string]string {
	return map[string]string{"user": "root", "database": "lm-prod", "application_name": "psql"}
}

// waitFor polls until cond holds, so a cell asserting on an event the server
// emits AFTER answering the client does not race the server's own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func kinds(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func find(events []Event, kind string) (Event, bool) {
	for _, e := range events {
		if e.Kind == kind {
			return e, true
		}
	}
	return Event{}, false
}

// MATRIX ROW 2.6: cleartext is offered, and it is the ONLY thing offered.
//
// The negative half is the point. Offering SCRAM alongside would be a menu
// item that fails for everyone who picks it, because a SCRAM verifier needs
// material the server deliberately does not keep — and a client that picks it
// gets an authentication failure it will read as a bad password.
// Matrix row 5:AuthenticationCleartextPassword: this cell proves the prompt.
func TestAuth_OffersCleartextAndNothingElse(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	_, fe := startupTo(t, addr, defaultParams())
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the auth request: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		t.Fatalf("the server offered %T; row 2.6 offers cleartext and only cleartext", msg)
	}
}

// MATRIX ROW 2.9's success sequence, in the protocol's order.
// Matrix row 5:AuthenticationCleartextPassword: this cell proves the grouped
// AuthenticationOk, ParameterStatus, BackendKeyData, and ReadyForQuery half.
func TestAuth_SuccessSequence(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	tc, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending the credential: %v", err)
	}

	var statuses []*pgproto3.ParameterStatus
	var key *pgproto3.BackendKeyData
	var sawOK bool
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("reading the success sequence: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.AuthenticationOk:
			sawOK = true
		case *pgproto3.ParameterStatus:
			if !sawOK {
				t.Fatal("a ParameterStatus arrived before AuthenticationOk")
			}
			statuses = append(statuses, &pgproto3.ParameterStatus{Name: m.Name, Value: m.Value})
		case *pgproto3.BackendKeyData:
			key = &pgproto3.BackendKeyData{ProcessID: m.ProcessID, SecretKey: append([]byte(nil), m.SecretKey...)}
		case *pgproto3.ReadyForQuery:
			if m.TxStatus != 'I' {
				t.Errorf("ReadyForQuery status = %q, want 'I' — a fresh session is idle, not in a transaction", m.TxStatus)
			}
			goto done
		default:
			t.Fatalf("unexpected %T in the success sequence", msg)
		}
	}
done:
	if !sawOK {
		t.Fatal("no AuthenticationOk")
	}
	if key == nil {
		t.Fatal("no BackendKeyData — a client with no cancel key cannot cancel anything")
	}
	// Four bytes, because every client here was negotiated down to 3.0 and
	// 3.0's cancel key is a fixed int32. pgproto3 models the field as a
	// []byte for 3.2, so nothing in the library would have stopped a longer
	// one going to a client that reads exactly four and then loses the frame
	// boundary for everything after it.
	// FOUR, as a literal. Comparing against CancelKeyLen would have been a
	// cell that reads `x == x`: changing the constant would change both
	// sides and the assertion would follow the bug it exists to catch. The
	// mutation battery found exactly that, and this is what it found.
	if len(key.SecretKey) != 4 {
		t.Errorf("cancel key is %d bytes, want 4 — every client here was negotiated down to "+
			"protocol 3.0, whose cancel key is a fixed int32, and a client that reads four "+
			"bytes of a longer one loses the frame boundary for everything after it",
			len(key.SecretKey))
	}

	got := map[string]string{}
	for _, ps := range statuses {
		got[ps.Name] = ps.Value
	}
	if got["is_superuser"] != "off" {
		t.Errorf("is_superuser = %q, want off — autodb's gates apply whatever the target would have said", got["is_superuser"])
	}
	if got["application_name"] != "psql" {
		t.Errorf("application_name = %q, want the startup echo %q", got["application_name"], "psql")
	}
	// The CANONICAL name, not the client's spelling. The startup said
	// "root"; the account is "Root", and the identity a session reports
	// should be the one the grants are written against.
	if got["session_authorization"] != "Root" {
		t.Errorf("session_authorization = %q, want the canonical %q", got["session_authorization"], "Root")
	}

	opened, _ := f.calls()
	if len(opened) != 1 || opened[0] != "autodb_pat_secret|root|lm-prod|127.0.0.1" {
		t.Errorf("the engine was asked %v; the credential, the startup user, the database and the "+
			"peer HOST should reach it exactly as presented", opened)
	}

	waitFor(t, "fd.session_open", func() bool { _, ok := find(events(), "fd.session_open"); return ok })
	ok, _ := find(events(), "fd.auth_ok")
	for _, want := range []string{"user=Root", "pat=laptop", "admitted-by=global"} {
		if !strings.Contains(ok.Detail, want) {
			t.Errorf("fd.auth_ok detail %q is missing %q — an operator cannot tell a login from "+
				"shared infrastructure from one from a person's own address without it", ok.Detail, want)
		}
	}

	_ = tc
}

// MATRIX ROW 2.7's refusal: whatever the internal cause, ONE wire shape.
func TestAuth_DenialIsUniformAndTheCauseIsAuditOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"a bad credential", exec.WireDenial(exec.DenyBadCredential), exec.DenyBadCredential},
		{"a user that is not the token's owner", exec.WireDenial(exec.DenyUserMismatch), exec.DenyUserMismatch},
		{"an address outside the admission set", exec.WireDenial(exec.DenyIPNotAdmitted), exec.DenyIPNotAdmitted},
		{"a token narrowed to other addresses", exec.WireDenial(exec.DenyPATIPNarrowed), exec.DenyPATIPNarrowed},
		{"a database that does not exist", exec.WireDenial(exec.DenyNoSuchDatabase), exec.DenyNoSuchDatabase},
		{"no grant on the target", exec.WireDenial(exec.DenyNoGrant), exec.DenyNoGrant},
		{"a target that refuses front-door use", exec.WireDenial(exec.DenyProfileRefuses), exec.DenyProfileRefuses},
		{"the target's leases are all spent", exec.WireDenial(exec.DenyLeaseCap), exec.DenyLeaseCap},
		{"the session cap", exec.WireDenial(exec.DenySessionCap), exec.DenySessionCap},
		{"the resident budget", exec.WireDenial(exec.DenyResidentBudget), exec.DenyResidentBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeAuth{err: tc.err}
			events, addr := authListener(t, f)

			conn, fe := startupTo(t, addr, defaultParams())
			if _, err := fe.Receive(); err != nil {
				t.Fatalf("auth request: %v", err)
			}
			fe.Send(&pgproto3.PasswordMessage{Password: "whatever"})
			if err := fe.Flush(); err != nil {
				t.Fatal(err)
			}
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("reading the denial: %v", err)
			}
			e, ok := msg.(*pgproto3.ErrorResponse)
			if !ok {
				t.Fatalf("got %T, want the uniform denial", msg)
			}
			if e.Code != DenialSQLState || e.Message != DenialMessage || e.Detail != "frontdoor/denied" {
				t.Errorf("the wire learned code=%q message=%q detail=%q; every cause must produce "+
					"the same three, or a caller can map the install by asking repeatedly",
					e.Code, e.Message, e.Detail)
			}
			if strings.Contains(e.Message+e.Detail+e.Hint, "grant") ||
				strings.Contains(e.Message+e.Detail+e.Hint, "database") {
				t.Errorf("the denial named the cause: %+v", e)
			}
			_ = conn

			waitFor(t, "the audit row", func() bool { _, ok := find(events(), "fd.auth_denied"); return ok })
			denied, _ := find(events(), "fd.auth_denied")
			if denied.Reason != tc.reason {
				t.Errorf("audited reason = %q, want %q — the trail is where the cause survives",
					denied.Reason, tc.reason)
			}
		})
	}
}

// A STORE failure is not a credential failure, and the difference is the
// number an operator watches for credential attacks.
//
// The wire shape is identical — telling a caller our database is unreachable
// is an answer they have not earned either — so the only place the
// distinction can live is the audit trail.
func TestAuth_StoreFailureIsNotACredentialFailure(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: errors.New("meta store: connection refused")}
	events, addr := authListener(t, f)

	_, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good-token"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok || e.Code != DenialSQLState {
		t.Fatalf("got %T/%v, want the uniform denial", msg, msg)
	}

	waitFor(t, "the audit row", func() bool { _, ok := find(events(), "fd.auth_denied"); return ok })
	denied, _ := find(events(), "fd.auth_denied")
	if denied.Reason != string(reasonAuthStoreError) {
		t.Errorf("audited reason = %q, want %q — filing our own outage under a credential cause "+
			"inflates the count an operator alerts on with events that are our fault",
			denied.Reason, reasonAuthStoreError)
	}
}

// MATRIX ROW 2.8: once cleartext is offered, EVERY type-`p` frame is a password.
//
// A client that speaks SASL at us anyway does not get a different path to
// probe; its SASLInitialResponse is simply a wrong password. That is not this
// implementation being clever — it is what the protocol says, and the cell
// exists because "there is no distinguishable SASL path" is a claim worth
// holding a library upgrade to.
func TestAuth_SASLShapedBytesAreJustAWrongPassword(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{err: exec.WireDenial(exec.DenyBadCredential)}
	_, addr := authListener(t, f)

	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	// A SASLInitialResponse's bytes, sent as the raw type-`p` frame a SASL
	// client would send.
	sasl := &pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256", Data: []byte("n,,n=,r=abcdefgh")}
	body, err := sasl.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := conn.Write(body); werr != nil {
		t.Fatal(werr)
	}
	msg, rerr := fe.Receive()
	if rerr != nil {
		t.Fatalf("reading the answer: %v", rerr)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok || e.Code != DenialSQLState {
		t.Fatalf("got %T; SASL-shaped bytes must reach the same uniform denial", msg)
	}
	opened, _ := f.calls()
	if len(opened) != 1 {
		t.Fatalf("the engine saw %d verifications, want exactly 1 — the SASL bytes are a password", len(opened))
	}
	// A PasswordMessage is bytes up to the first NUL, and a
	// SASLInitialResponse opens with its mechanism name and a NUL — so the
	// mechanism IS the password the engine was asked to verify. That is the
	// whole of row 2.8: not a SASL path this code declines to take, but no
	// SASL path existing to take.
	if !strings.HasPrefix(opened[0], "SCRAM-SHA-256|") {
		t.Errorf("the engine was handed %q; the SASL frame's mechanism name is what a "+
			"PasswordMessage decode yields from those bytes", opened[0])
	}
}

// MATRIX ROW 2.8, the other half: a frame that is NOT type-`p` before
// authentication is a protocol violation, and it must not reach the engine.
func TestAuth_ANonPasswordFrameNeverReachesTheEngine(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.Query{String: "select 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if e, ok := msg.(*pgproto3.ErrorResponse); !ok || e.Code != DenialSQLState {
		t.Fatalf("got %T, want the uniform denial", msg)
	}
	_ = conn

	opened, _ := f.calls()
	if len(opened) != 0 {
		t.Errorf("the engine ran %d verifications for a Query sent before authentication; "+
			"a frame that is not a credential must not become one", len(opened))
	}
	waitFor(t, "the audit row", func() bool { _, ok := find(events(), "fd.auth_denied"); return ok })
	denied, _ := find(events(), "fd.auth_denied")
	if denied.Reason != string(reasonPreAuthProtocolViolation) {
		t.Errorf("audited reason = %q, want %q", denied.Reason, reasonPreAuthProtocolViolation)
	}
}

// A startup that failed policy NEVER reaches the credential exchange.
//
// Offering the password prompt to a connection already destined for refusal
// would invite a peer to send a real token we then have to be careful not to
// have learned anything from — and it would put a credential on the wire for
// a connection that was never going to be served.
func TestAuth_ARefusedStartupIsNeverOfferedThePrompt(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	// `search_path` is refused by §3.1.
	_, fe := startupTo(t, addr, map[string]string{
		"user": "root", "database": "lm-prod", "search_path": "public",
	})
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); ok {
		t.Fatal("the server offered a password prompt to a startup it had already refused")
	}
	if e, ok := msg.(*pgproto3.ErrorResponse); !ok || e.Code != DenialSQLState {
		t.Fatalf("got %T, want the uniform denial", msg)
	}
	opened, _ := f.calls()
	if len(opened) != 0 {
		t.Errorf("the engine was consulted %d times for a startup that never passed policy", len(opened))
	}
}

// Row 2.7's reservation is released however the connection ends.
//
// The reservation has four members and it is taken on the engine's side; if
// the wire forgets to release it, the caps leak one slot per connection and
// the front door slowly stops admitting anyone. Nothing about that failure is
// visible at the time it happens, which is exactly why it needs a cell.
func TestAuth_TheReservationIsReleasedWhenTheClientLeaves(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("success sequence: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	fe.Send(&pgproto3.Terminate{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	waitFor(t, "the session to be released", func() bool {
		_, closed := f.calls()
		return len(closed) == 1
	})
	_, closed := f.calls()
	if closed[0] != "sess-abc123/peer-closed" {
		t.Errorf("released %q, want the session id and a reason", closed[0])
	}
}

// A shutdown releases its sessions with a LIVE context.
//
// The commonest reason a session is released is that the listener is
// stopping, and stopping is what cancels the context the release runs under.
// Handing that context to the teardown means the audit row for why the
// session ended — and, once F1 lands, the rollback of what it was holding —
// is attempted against a context that is already dead. The failure is
// invisible at the time and shows up as a gap in the trail exactly when
// someone is reconstructing a shutdown.
//
// The cell owns its own context and CANCELS it, because that is the actual
// shutdown path: the daemon stops by cancelling the context it passed to
// Serve. An earlier version called Close() instead — which ends the
// connections but cancels nothing — so the context under test was never
// cancelled and the assertion held whichever way the code went. It took a
// mutation to show that; a green cell that cannot fail is worse than no cell,
// because it is counted.
func TestSession_ShutdownReleasesWithALiveContext(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{Authn: f, AuthFailuresPerIP: unthrottled})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); l.Close() }()
	go func() { _ = l.Serve(ctx) }()

	conn, _ := authenticated(t, l.Addr().String())
	defer func() { _ = conn.Close() }()

	// The shutdown, exactly as the daemon performs it.
	cancel()

	waitFor(t, "the session to be released", func() bool {
		_, closed := f.calls()
		return len(closed) == 1
	})
	for i, cerr := range f.closeContexts() {
		if cerr != nil {
			t.Errorf("release %d ran under a context that was already %v; the shutdown cancelled "+
				"the very context its own teardown needed", i, cerr)
		}
	}
}

// THE AUTHENTICATOR IS BOUND BY THE PHASE'S DEADLINE (lector PR #36 r1).
//
// It used to receive the listener's context, which has no deadline at all —
// so a stuck auth store held a credential worker indefinitely, and nothing
// upstream was ever going to cancel it. Sixteen such calls and the surface
// stops authenticating anyone, with no timeout anywhere to end it.
func TestAuthDeadline_TheAuthenticatorSeesThePhaseDeadline(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, _, addr := listenerWith(t, Options{
		Authn: f, AuthFailuresPerIP: unthrottled,
		testDeadlines: &deadlines{tls: 5 * time.Second, startup: 5 * time.Second,
			auth: 700 * time.Millisecond, idle: time.Minute},
	})

	before := time.Now()
	_, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the verification", func() bool { o, _ := f.calls(); return len(o) == 1 })

	dls, oks := f.authDeadlines()
	if len(oks) != 1 || !oks[0] {
		t.Fatal("the authenticator was handed a context with NO deadline. A store that will " +
			"not answer then holds a credential worker forever, and sixteen of those stop the " +
			"surface authenticating anyone with no timeout anywhere to end it")
	}
	// Within the phase's budget, measured from before the exchange began.
	if got := dls[0].Sub(before); got > 700*time.Millisecond+250*time.Millisecond || got <= 0 {
		t.Errorf("the verification's deadline is %v from the start of the exchange, want at "+
			"most the phase's %v — a deadline larger than the phase is a second budget wearing "+
			"the first one's name", got, 700*time.Millisecond)
	}
}

// A SATURATED QUEUE DOES NOT HAND OUT A SECOND BUDGET.
//
// The waiter used to start a fresh full timer, so a peer who had already spent
// the allowance getting to the queue could wait the whole allowance again.
// Lector measured 502.96ms against a 300ms setting: 200ms before the password,
// then 302.96ms waiting for a worker.
//
// The discriminating measurement is the time from the PASSWORD, not from the
// prompt. A cell that only bounded the total would pass on a fresh timer
// whenever the client happened to be quick.
func TestAuthDeadline_ASaturatedQueueExitsAtTheOriginalDeadline(t *testing.T) {
	t.Parallel()
	const authBudget = 500 * time.Millisecond
	const clientDelay = 250 * time.Millisecond

	hold := make(chan struct{})
	entered := make(chan struct{}, 4)
	blocking := &holdingAuth{entered: entered, release: hold}

	_, _, addr := listenerWith(t, Options{
		Authn: blocking, AuthFailuresPerIP: unthrottled,
		AuthWorkers: 1, // one worker, so the second caller queues
		testDeadlines: &deadlines{tls: 5 * time.Second, startup: 5 * time.Second,
			auth: authBudget, idle: time.Minute},
	})
	// REGISTERED AFTER THE LISTENER, so LIFO runs it BEFORE the listener's
	// own cleanup. Close now waits for in-flight handlers, and this holder
	// deliberately ignores its context — so releasing it second would leave
	// Close waiting on a goroutine nothing was going to free. The join being
	// real is what makes the order matter.
	t.Cleanup(func() { close(hold) })

	// The first connection takes the only worker and holds it.
	_, feA := startupTo(t, addr, defaultParams())
	if _, err := feA.Receive(); err != nil {
		t.Fatal(err)
	}
	feA.Send(&pgproto3.PasswordMessage{Password: "first"})
	if err := feA.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first connection never reached the verification, so the worker is not held")
	}

	// The second spends part of its budget before presenting anything.
	connB, feB := startupTo(t, addr, defaultParams())
	if _, err := feB.Receive(); err != nil {
		t.Fatal(err)
	}
	promptAt := time.Now()
	time.Sleep(clientDelay)
	feB.Send(&pgproto3.PasswordMessage{Password: "second"})
	if err := feB.Flush(); err != nil {
		t.Fatal(err)
	}
	passwordAt := time.Now()

	_ = connB.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, _ = io.ReadAll(connB)
	sincePrompt, sincePassword := time.Since(promptAt), time.Since(passwordAt)
	t.Logf("closed %v after the prompt, %v after the password (budget %v, client delay %v)",
		sincePrompt, sincePassword, authBudget, clientDelay)

	// THE DISCRIMINATOR. A fresh timer would give this connection the whole
	// budget again from the password onwards.
	if sincePassword >= authBudget {
		t.Errorf("the connection survived %v after presenting its credential, which is the "+
			"whole %v budget over again. The phase's allowance is a property of the phase: a "+
			"peer who spent %v of it before answering does not get it back by having waited",
			sincePassword, authBudget, clientDelay)
	}
	if sincePrompt > authBudget+400*time.Millisecond {
		t.Errorf("the exchange ran %v against a %v budget", sincePrompt, authBudget)
	}
}

// holdingAuth occupies a credential worker until the TEST releases it, and
// deliberately ignores its context.
//
// A store that honours cancellation would give the slot back the moment the
// first caller's own budget expired — and then the second caller's wait would
// be bounded by the FIRST caller's deadline rather than by its own, which is
// not the property under test. The first version did exactly that and the
// cell measured a coincidence: both numbers landed where they would have
// anyway.
//
// It is also the honest model of the case that motivated the deadline. A
// store that answers cancellation promptly was never the problem; a stuck one
// is, and a stuck one does not notice a context.
type holdingAuth struct {
	entered chan struct{}
	release chan struct{}
}

func (h *holdingAuth) OpenWireSession(context.Context, string, string, string, string) (exec.WireSessionResult, error) {
	h.entered <- struct{}{}
	<-h.release
	return goodSession(), nil
}

func (h *holdingAuth) CloseWireSession(context.Context, exec.SessionID, int64, string, string) {}

// OUR CAPACITY IS NOT THEIR CREDENTIAL (lector PR #36 r1, attribution).
//
// A peer that waited for a credential worker we could not spare presented
// something we never looked at. Charging that to their address throttles them
// for OUR shortfall — the same mistake as throttling one for our own store
// outage, and worse under load, because the moment the surface is busiest is
// exactly when it would start locking out the clients waiting for it.
func TestAuthDeadline_AWorkerTimeoutIsNotChargedToTheSource(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	entered := make(chan struct{}, 4)
	blocking := &holdingAuth{entered: entered, release: hold}

	// A throttle of exactly the matrix minimum, so ONE wrongly-charged
	// failure is enough to close the door on the next connection.
	_, events, addr := listenerWith(t, Options{
		Authn: blocking, AuthWorkers: 1,
		testDeadlines: &deadlines{tls: 5 * time.Second, startup: 5 * time.Second,
			auth: 300 * time.Millisecond, idle: time.Minute},
	})
	t.Cleanup(func() { close(hold) }) // see above: before the listener's cleanup

	// Occupy the only worker.
	_, feA := startupTo(t, addr, defaultParams())
	if _, err := feA.Receive(); err != nil {
		t.Fatal(err)
	}
	feA.Send(&pgproto3.PasswordMessage{Password: "first"})
	if err := feA.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker was never taken")
	}

	// Enough queued-and-timed-out attempts to trip the throttle several
	// times over, if any of them were being charged.
	// TOLERANT of a refusal rather than fatal on it: being refused IS the
	// defect, and a helper that calls Fatal on the way in would report it as
	// a broken harness. The first version did exactly that — the right
	// verdict with a message pointing at the wrong thing.
	attempts := AuthFailuresPerIP + 2
	refusedAt := 0
	for i := 1; i <= attempts; i++ {
		if !queueOneAttempt(t, addr) {
			refusedAt = i
			break
		}
	}

	if refusedAt > 0 {
		t.Fatalf("attempt %d was refused at accept, after %d timeouts waiting for OUR credential "+
			"workers. Under load that turns a capacity shortfall into a lockout of exactly the "+
			"clients queueing for it", refusedAt, refusedAt-1)
	}
	for _, e := range events() {
		if e.Kind == "fd.budget_refuse" && e.Reason == string(reasonSourceThrottled) {
			t.Fatalf("the source was throttled after %d timeouts waiting for OUR credential "+
				"workers", attempts)
		}
	}
}

// queueOneAttempt drives one connection to the credential queue and waits for
// it to be dropped. It reports false when the listener refused the connection
// outright, which is the throttle rather than the timeout.
func queueOneAttempt(t *testing.T, addr string) bool {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	if _, werr := raw.Write(sslRequest()); werr != nil {
		return false
	}
	answer := make([]byte, 1)
	if n, rerr := raw.Read(answer); rerr != nil || n != 1 || answer[0] != 'S' {
		return false // refused at accept: no SSL answer
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // not the subject
	if herr := tc.Handshake(); herr != nil {
		return false
	}
	if _, werr := tc.Write(startupPacket(protocolVersion30, defaultParams())); werr != nil {
		return false
	}
	fe := pgproto3.NewFrontend(tc, tc)
	if _, rerr := fe.Receive(); rerr != nil {
		return false
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "queued"})
	if ferr := fe.Flush(); ferr != nil {
		return false
	}
	_, _ = io.ReadAll(tc)
	return true
}

// CLOSE JOINS THE HANDLERS, including a session still tearing down (lector
// PR #38 r0 must-fix 2).
//
// Close's own comment promised this and the code never did it: only Serve
// waited, in a goroutine the daemon starts and discards. So Close returned
// while an authenticated handler was still inside CloseWireSession, and the
// engine teardown the daemon runs next could race the wire teardown it was
// deliberately ordered after.
//
// The cell blocks a session's release and requires Close to be blocked with
// it. Both halves matter: that it does NOT return while the teardown is in
// flight, and that it DOES return once the teardown finishes — an
// implementation that simply never returned would satisfy the first alone.
func TestListenerClose_WaitsForASessionStillTearingDown(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{})
	f := &blockingCloseAuth{result: goodSession(), entered: entered, release: release}

	l, _, addr := listenerWith(t, Options{Authn: f, AuthFailuresPerIP: unthrottled})

	conn, fe := startupTo(t, addr, defaultParams())
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "good"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		msg, rerr := fe.Receive()
		if rerr != nil {
			t.Fatalf("the success sequence: %v", rerr)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	// The client goes away, which sends the handler into its teardown — and
	// the fake holds it there.
	_ = conn.Close()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the session never reached its teardown; the cell cannot observe the join")
	}

	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a session was still tearing down. Its contract says it " +
			"waits for in-flight connections, and the daemon tears the ENGINE down immediately " +
			"after — so the release still running here would be racing the pools it releases into")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned once the teardown finished")
	}
}

// blockingCloseAuth holds CloseWireSession open until released.

// blockingCloseAuth holds CloseWireSession open until released.
type blockingCloseAuth struct {
	result  exec.WireSessionResult
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingCloseAuth) OpenWireSession(context.Context, string, string, string, string) (exec.WireSessionResult, error) {
	return b.result, nil
}

func (b *blockingCloseAuth) CloseWireSession(context.Context, exec.SessionID, int64, string, string) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
}
