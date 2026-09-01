package frontdoor

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// liveListener starts a real listener on a real port with real TLS material.
// Nothing here is faked: the whole subject is what happens on a socket.
// The events accessor is a FUNCTION, not the slice. Returning the slice
// returns a header captured at that moment, so the callback's later appends
// are invisible to the caller — my first version did exactly that and read
// an empty list while the listener was emitting events correctly. An
// assertion of "no events" would have passed forever.
func liveListener(t testing.TB) (*Listener, func() []Event, string) {
	t.Helper()
	return listenerWith(t, Options{})
}

// unthrottled is the per-source allowance for harnesses that make many
// refused connections from 127.0.0.1 in one test.
//
// Raised, never removed, and only where the throttle is not the subject: a
// timing harness that takes thirty samples would otherwise be measuring how
// fast the rate limiter closes a socket rather than how fast the denial path
// answers, and would report a beautifully uniform result about the wrong
// thing. The throttle has its own cells.
const unthrottled = 1 << 20

// listenerWith starts a real listener on a real port with real TLS material,
// merging the caller's options over the test defaults.
func listenerWith(t testing.TB, opt Options) (*Listener, func() []Event, string) {
	t.Helper()
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatalf("test material: %v", err)
	}
	var mu sync.Mutex
	var events []Event
	inner := opt.OnEvent
	opt.OnEvent = func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		if inner != nil {
			inner(e)
		}
	}
	l, err := Open("127.0.0.1:0", cfg, opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); l.Close() })
	snapshot := func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
	return l, snapshot, l.Addr().String()
}

func dial(t testing.TB, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

// sslRequest is the 8-byte SSLRequest packet.
func sslRequest() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], 8)
	binary.BigEndian.PutUint32(b[4:8], 80877103)
	return b
}

func startupPacket(version uint32, params map[string]string) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, version)
	for k, v := range params {
		body = append(body, []byte(k)...)
		body = append(body, 0)
		body = append(body, []byte(v)...)
		body = append(body, 0)
	}
	body = append(body, 0)
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

// readErrorResponse reads one backend message and requires it to be the
// uniform denial.
func readDenial(t testing.TB, r net.Conn) *pgproto3.ErrorResponse {
	t.Helper()
	fe := pgproto3.NewFrontend(r, r)
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("reading the denial: %v", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected an ErrorResponse, got %T", msg)
	}
	return e
}

// TLS is not optional and there is no plaintext fallback (matrix row 2.1).
func TestStartup_PlaintextIsRefused(t *testing.T) {
	t.Parallel()
	_, events, addr := liveListener(t)

	c := dial(t, addr)
	if _, err := c.Write(startupPacket(protocolVersion30, map[string]string{"user": "root", "database": "d"})); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := readDenial(t, c)
	if e.Code != DenialSQLState || e.Message != DenialMessage {
		t.Fatalf("plaintext startup got %s/%q, want the uniform denial %s/%q",
			e.Code, e.Message, DenialSQLState, DenialMessage)
	}

	// The wire shape ALONE cannot prove this, and asserting only the shape
	// was a blind cell: every path on this listener ends in the same denial,
	// so a version that ACCEPTED the plaintext startup and then denied for
	// want of a credential store produced byte-identical output. It passed
	// against a mutation that removed the refusal entirely.
	//
	// What distinguishes them is the internal reason and the absence of TLS.
	// Once the auth chain lands, the accepting version would read a
	// cleartext password off an unencrypted socket while this assertion
	// still passed — which is the whole hazard.
	var reason string
	var sawTLS bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range events() {
			if ev.Kind == "fd.auth_denied" {
				reason = ev.Reason
			}
			if ev.Kind == "fd.tls_ok" {
				sawTLS = true
			}
		}
		if reason != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason != reasonPlaintextStartup.String() {
		t.Errorf("the denial was audited as %q, want %q — the startup was not refused FOR being "+
			"plaintext, it merely failed for some other reason on its way past",
			reason, reasonPlaintextStartup)
	}
	if sawTLS {
		t.Error("TLS was established on a connection that never asked for it")
	}
}

// GSS encryption is refused with 'N' (row 2.2) — the protocol's own way of
// saying no, so a client can proceed to ask for TLS instead of failing.
func TestStartup_GSSEncIsRefusedWithN(t *testing.T) {
	t.Parallel()
	_, _, addr := liveListener(t)

	c := dial(t, addr)
	pkt := make([]byte, 8)
	binary.BigEndian.PutUint32(pkt[0:4], 8)
	binary.BigEndian.PutUint32(pkt[4:8], 80877104) // GSSENCRequest
	if _, err := c.Write(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("reading the GSS answer: %v", err)
	}
	if buf[0] != 'N' {
		t.Fatalf("GSSENCRequest answered %q, want 'N'", buf[0])
	}
}

// The happy path as far as this slice goes: TLS is established, the startup
// packet is accepted, and the connection is then denied because no
// credential store exists yet. The denial is the honest state; the TLS
// handshake succeeding is what this cell actually proves.
func TestStartup_TLSEstablishesThenDeniesUniformly(t *testing.T) {
	t.Parallel()
	_, events, addr := liveListener(t)

	c := dial(t, addr)
	if _, err := c.Write(sslRequest()); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	answer := make([]byte, 1)
	if _, err := c.Read(answer); err != nil {
		t.Fatalf("reading the SSL answer: %v", err)
	}
	if answer[0] != 'S' {
		t.Fatalf("SSLRequest answered %q, want 'S'", answer[0])
	}

	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the CA is not the subject here
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if _, err := tc.Write(startupPacket(protocolVersion30, map[string]string{
		"user": "root", "database": "lm-prod",
	})); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	e := readDenial(t, tc)
	if e.Code != DenialSQLState {
		t.Errorf("code = %s, want %s", e.Code, DenialSQLState)
	}

	// The audit trail records the internal reason the wire withheld.
	var sawTLS, sawDenied bool
	var seen []Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		seen = events()
		sawTLS, sawDenied = false, false
		for _, ev := range seen {
			if ev.Kind == "fd.tls_ok" {
				sawTLS = true
			}
			if ev.Kind == "fd.auth_denied" {
				sawDenied = true
			}
		}
		if sawTLS && sawDenied {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, ev := range seen {
		if ev.Kind == "fd.auth_denied" {
			if ev.Reason == "" {
				t.Error("fd.auth_denied carries no internal reason; the audit trail is as blind " +
					"as the wire, and the uniform denial is only safe because the trail is not")
			}
		}
	}
	if !sawTLS || !sawDenied {
		t.Errorf("events missing (tls_ok=%v denied=%v): %+v", sawTLS, sawDenied, seen)
	}
}

// A 3.x minor negotiates DOWN and continues (row 2.5); an unsupported major
// is refused (row 2.5a). PostgreSQL itself does the former, and a hard
// refusal would break newer clients perfectly able to speak 3.0.
func TestStartup_VersionNegotiation(t *testing.T) {
	t.Parallel()

	t.Run("a 3.x minor is negotiated down, not refused", func(t *testing.T) {
		t.Parallel()
		_, _, addr := liveListener(t)
		tc := tlsDial(t, addr)
		if _, err := tc.Write(startupPacket(uint32(3)<<16|2, map[string]string{"user": "root", "database": "d"})); err != nil {
			t.Fatal(err)
		}
		fe := pgproto3.NewFrontend(tc, tc)
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		if _, ok := msg.(*pgproto3.NegotiateProtocolVersion); !ok {
			t.Fatalf("got %T, want NegotiateProtocolVersion — a 3.x client that can speak 3.0 "+
				"must be negotiated down rather than refused", msg)
		}
	})

	t.Run("an unsupported major is refused", func(t *testing.T) {
		t.Parallel()
		_, _, addr := liveListener(t)
		tc := tlsDial(t, addr)
		if _, err := tc.Write(startupPacket(uint32(4)<<16|0, map[string]string{"user": "root", "database": "d"})); err != nil {
			t.Fatal(err)
		}
		e := readDenial(t, tc)
		if e.Code != DenialSQLState {
			t.Errorf("code = %s, want the uniform denial %s", e.Code, DenialSQLState)
		}
	})
}

func tlsDial(t *testing.T, addr string) *tls.Conn {
	t.Helper()
	c := dial(t, addr)
	if _, err := c.Write(sslRequest()); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err := c.Read(answer); err != nil || answer[0] != 'S' {
		t.Fatalf("SSL answer %q err %v", answer, err)
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // not the subject
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return tc
}

// Row 2.5 also requires the NegotiateProtocolVersion to NAME the `_pq_.*`
// options it did not understand. Silence would leave a client believing an
// extension it depends on had been accepted — the negotiation exists to be
// specific, not merely to say "3.0".
func TestStartup_UnrecognizedProtocolOptionsAreNamed(t *testing.T) {
	t.Parallel()
	_, _, addr := liveListener(t)

	tc := tlsDial(t, addr)
	if _, err := tc.Write(startupPacket(protocolVersion30, map[string]string{
		"user":               "root",
		"_pq_.something":     "1",
		"_pq_.another_thing": "on",
	})); err != nil {
		t.Fatal(err)
	}
	fe := pgproto3.NewFrontend(tc, tc)
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	n, ok := msg.(*pgproto3.NegotiateProtocolVersion)
	if !ok {
		t.Fatalf("got %T, want NegotiateProtocolVersion — a client asking for protocol "+
			"extensions we do not implement must be told which ones", msg)
	}
	if len(n.UnrecognizedOptions) != 2 {
		t.Fatalf("UnrecognizedOptions = %v, want both _pq_ options named", n.UnrecognizedOptions)
	}
	// Sorted, so the message is deterministic and testable at all.
	if n.UnrecognizedOptions[0] != "_pq_.another_thing" || n.UnrecognizedOptions[1] != "_pq_.something" {
		t.Errorf("UnrecognizedOptions = %v, want them sorted", n.UnrecognizedOptions)
	}
}

// MF1. Row 2.2 refuses GSS encryption with 'N' and lets the client CARRY ON:
// 'N' is the protocol's own way of declining an option, and libpq's next move
// after it is to ask for TLS. Answering 'N' and hanging up turned a declined
// option into a dead connection, so a client that tried GSS first could never
// reach TLS at all — while this file's own prose claimed otherwise.
func TestStartup_GSSRefusalLetsTheClientProceedToTLS(t *testing.T) {
	t.Parallel()
	_, _, addr := liveListener(t)

	c := dial(t, addr)
	gss := make([]byte, 8)
	binary.BigEndian.PutUint32(gss[0:4], 8)
	binary.BigEndian.PutUint32(gss[4:8], 80877104)
	if _, err := c.Write(gss); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err := c.Read(answer); err != nil || answer[0] != 'N' {
		t.Fatalf("GSS answer = %q err %v, want 'N'", answer, err)
	}

	// THE POINT: the connection is still usable. Ask for TLS.
	if _, err := c.Write(sslRequest()); err != nil {
		t.Fatalf("the connection was closed after the GSS refusal: %v", err)
	}
	if _, err := c.Read(answer); err != nil {
		t.Fatalf("no answer to SSLRequest after a GSS refusal: %v", err)
	}
	if answer[0] != 'S' {
		t.Fatalf("SSLRequest after GSS answered %q, want 'S' — declining one option must not "+
			"end the connection, or a client that tries GSS first can never reach TLS", answer[0])
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // not the subject
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake after a GSS refusal: %v", err)
	}
}

// MF2. Direct TLS is a TLS failure, not an authentication denial — the peer
// never presented a credential. Calling it fd.auth_denied puts a
// non-authentication event in the trail an operator reads to count credential
// attacks, and writing a PostgreSQL error frame to a client speaking raw TLS
// is noise on the wire.
func TestStartup_DirectTLSIsATLSFailureNotAnAuthDenial(t *testing.T) {
	t.Parallel()
	_, events, addr := liveListener(t)

	c := dial(t, addr)
	// A TLS ClientHello, which is what sslnegotiation=direct sends first.
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"postgresql"}}) //nolint:gosec // not the subject
	_ = tc.Handshake()                                                                             // expected to fail; the server refuses

	var kinds []string
	var reason string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kinds = kinds[:0]
		for _, ev := range events() {
			kinds = append(kinds, ev.Kind)
			if ev.Kind == "fd.tls_fail" {
				reason = ev.Reason
			}
		}
		if reason != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason == "" {
		t.Fatalf("direct TLS emitted no fd.tls_fail; events were %v", kinds)
	}
	if reason != "direct-tls-unsupported" {
		t.Errorf("fd.tls_fail reason = %q, want direct-tls-unsupported", reason)
	}
	for _, k := range kinds {
		if k == "fd.auth_denied" {
			t.Error("direct TLS was audited as an AUTHENTICATION denial; no credential was ever " +
				"presented, and the auth trail is what an operator counts credential attacks in")
		}
	}
}

// MF3. §3.1's accepted set is CLOSED. A parameter outside it is refused FOR
// being refused — not allowed to fall through to whatever denies next.
// MATRIX ROW 2.4: the StartupMessage's parameters are pinned by §3.1's closed
// set — a parameter not named there is refused rather than emulated as a GUC.
func TestStartup_ParameterPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		params map[string]string
		refuse bool
	}{
		{"the pinned set", map[string]string{
			"user": "root", "database": "lm-prod",
			"application_name": "psql", "client_encoding": "UTF8",
		}, false},
		{"an unknown parameter is a GUC attempt", map[string]string{
			"user": "root", "database": "d", "search_path": "public",
		}, true},
		{"replication is refused at any value", map[string]string{
			"user": "root", "database": "d", "replication": "database",
		}, true},
		{"a non-UTF8 client_encoding", map[string]string{
			"user": "root", "database": "d", "client_encoding": "LATIN1",
		}, true},
		{"UTF-8 spelled with a hyphen is still UTF8", map[string]string{
			"user": "root", "database": "d", "client_encoding": "utf-8",
		}, false},
		{"options that sets a GUC", map[string]string{
			"user": "root", "database": "d", "options": "-c search_path=public",
		}, true},
		{"options in the --key=val spelling", map[string]string{
			"user": "root", "database": "d", "options": "--search_path=public",
		}, true},
		{"empty options is accepted and ignored", map[string]string{
			"user": "root", "database": "d", "options": "   ",
		}, false},
		{"_pq_ extensions are negotiated, not refused", map[string]string{
			"user": "root", "database": "d", "_pq_.some_extension": "1",
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refused, ok := checkStartupParams(tc.params)
			if tc.refuse && ok {
				t.Errorf("%v was accepted; PostgreSQL treats an unknown startup parameter as a "+
					"GUC attempt, and a client-controlled GUC must not reach the pinned target",
					tc.params)
			}
			if !tc.refuse && !ok {
				t.Errorf("%v was refused as %q", tc.params, refused)
			}
		})
	}
}

// And the refusal reaches the wire as the uniform denial while the AUDIT
// names the parameter — the caller learns that startup failed, not which
// parameter this server dislikes.
func TestStartup_RefusedParameterIsAuditedButNotDisclosed(t *testing.T) {
	t.Parallel()
	_, events, addr := liveListener(t)

	tc := tlsDial(t, addr)
	if _, err := tc.Write(startupPacket(protocolVersion30, map[string]string{
		"user": "root", "database": "lm-prod", "search_path": "public",
	})); err != nil {
		t.Fatal(err)
	}
	e := readDenial(t, tc)
	if e.Code != DenialSQLState || e.Message != DenialMessage {
		t.Fatalf("got %s/%q, want the uniform denial", e.Code, e.Message)
	}
	if strings.Contains(strings.ToLower(e.Message+e.Detail), "search_path") {
		t.Error("the wire names the refused parameter, which maps the accepted set for anyone " +
			"willing to ask repeatedly")
	}

	var reason, detail string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && reason == "" {
		for _, ev := range events() {
			if ev.Kind == "fd.auth_denied" {
				reason, detail = ev.Reason, ev.Detail
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason != reasonStartupParamRefus.String() {
		t.Errorf("audited reason = %q, want %q — a refused parameter must be refused FOR that, "+
			"not left to fall through to whatever denies next", reason, reasonStartupParamRefus)
	}
	// And the PARAMETER NAME reaches the audit. The previous version carried
	// a RefusedParam field, commented that it was for the audit row, and
	// dropped it — the test asserted only the generic reason, so the claim
	// and the code disagreed with nothing to catch it.
	if detail != "search_path" {
		t.Errorf("audited detail = %q, want the refused parameter named; an operator reading "+
			"this cannot tell WHICH parameter was refused, which is the one thing the audit "+
			"is for here", detail)
	}
}

// MF1. An oversized length-prefixed frame is NOT a direct-TLS attempt. The
// first version inferred direct TLS from pgproto3's "invalid length" error,
// which a TLS ClientHello does produce — and so does an ordinary frame that
// simply exceeds the pre-auth cap. A client sending something too big was
// audited as a PostgreSQL 17 client, sending an operator to look for
// something that may not exist on their network.
// S0 frames are classified by their DECLARED LENGTH, and the three outcomes
// are distinct. Two rounds of review found the same shape of error here: a
// symptom shared by several causes ("invalid length") read as one specific
// cause. Underlength is malformed, over-cap is too large, and calling the
// first "too large" points an operator in the opposite direction.
func TestStartup_LengthClassification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		declared uint32
		want     string
	}{
		{"a length of zero", 0, reasonStartupMalformed.String()},
		{"a length below the four bytes a version needs", 3, reasonStartupMalformed.String()},
		{"a length of exactly the header", 4, reasonStartupMalformed.String()},
		{"an over-cap length", uint32(PreAuthMaxBodyLen + 5), reasonPreAuthOversize.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, events, addr := liveListener(t)
			c := dial(t, addr)
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], tc.declared)
			if _, err := c.Write(buf[:]); err != nil {
				t.Fatal(err)
			}
			var reason string
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && reason == "" {
				for _, ev := range events() {
					if ev.Kind == "fd.tls_fail" {
						reason = ev.Reason
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			if reason != tc.want {
				t.Errorf("a declared length of %d audited as %q, want %q", tc.declared, reason, tc.want)
			}
		})
	}

	// The unit is asserted directly too, so the boundary is pinned without
	// standing up a listener for each case.
	if _, bad := classifyStartupLength(uint32(PreAuthMaxBodyLen + 4)); bad {
		t.Error("a body of exactly the cap was rejected; the bound is inclusive")
	}
	if _, bad := classifyStartupLength(8); bad {
		t.Error("an ordinary 8-byte request (SSLRequest's size) was rejected")
	}
}

func TestStartup_OversizeIsNotDirectTLS(t *testing.T) {
	t.Parallel()
	_, events, addr := liveListener(t)

	c := dial(t, addr)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(PreAuthMaxBodyLen+5))
	if _, err := c.Write(lenBuf[:]); err != nil {
		t.Fatal(err)
	}

	var reason string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && reason == "" {
		for _, ev := range events() {
			if ev.Kind == "fd.tls_fail" {
				reason = ev.Reason
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason == "" {
		t.Fatal("an oversized S0 frame produced no fd.tls_fail event")
	}
	if reason == "direct-tls-unsupported" {
		t.Fatalf("an oversized length-prefixed frame was audited as direct TLS; it is not a "+
			"protocol negotiation at all, and the reason sends an operator hunting a "+
			"PostgreSQL 17 client that may not exist (reason=%q)", reason)
	}
	if reason != reasonPreAuthOversize.String() {
		t.Errorf("reason = %q, want %q", reason, reasonPreAuthOversize)
	}
}

// MF2. `user` and `database` are REQUIRED (§3.1). Without the check, an empty
// parameter map sailed through to be denied for want of a credential store —
// which reads in the audit as an authentication problem rather than as the
// malformed startup it is.
func TestStartup_RequiredParameters(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"no parameters at all", map[string]string{}, "user"},
		{"only user", map[string]string{"user": "root"}, "database"},
		{"only database", map[string]string{"database": "lm-prod"}, "user"},
		{"a blank user", map[string]string{"user": "  ", "database": "lm-prod"}, "user"},
		{"a blank database", map[string]string{"user": "root", "database": ""}, "database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refused, ok := checkStartupParams(tc.params)
			if ok {
				t.Fatalf("%v was accepted; both user and database are required", tc.params)
			}
			if refused != tc.want {
				t.Errorf("refused %q, want %q named", refused, tc.want)
			}
		})
	}

	// Positive control: both present is accepted, so the requirement is not
	// simply refusing everything.
	if _, ok := checkStartupParams(map[string]string{"user": "root", "database": "lm-prod"}); !ok {
		t.Error("a startup with both required parameters was refused")
	}
}
