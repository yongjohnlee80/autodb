package frontdoor

import (
	"context"
	"crypto/tls"
	"errors"
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

	// opened and closed record the calls, so a cell can assert the
	// reservation was released rather than assume it.
	opened []string // the presented credentials, in order
	closed []string // "<session-id>/<reason>"
	// closeCtxErr records the state of the context the release was handed,
	// so a cell can prove the teardown was not given a context that had
	// already been cancelled by the very shutdown that triggered it.
	closeCtxErr []error
}

func (f *fakeAuth) OpenWireSession(_ context.Context, presented, startupUser, database, ip string) (exec.WireSessionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
