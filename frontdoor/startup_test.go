package frontdoor

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
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
func liveListener(t *testing.T) (*Listener, func() []Event, string) {
	t.Helper()
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatalf("test material: %v", err)
	}
	var mu sync.Mutex
	var events []Event
	l, err := Open("127.0.0.1:0", cfg, Options{OnEvent: func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}})
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

func dial(t *testing.T, addr string) net.Conn {
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
func readDenial(t *testing.T, r net.Conn) *pgproto3.ErrorResponse {
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
	if _, err := c.Write(startupPacket(protocolVersion30, map[string]string{"user": "root"})); err != nil {
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
		if _, err := tc.Write(startupPacket(uint32(3)<<16|2, map[string]string{"user": "root"})); err != nil {
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
		if _, err := tc.Write(startupPacket(uint32(4)<<16|0, map[string]string{"user": "root"})); err != nil {
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
