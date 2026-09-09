package frontdoor

// THE NEGATIVE SPACE ON THE WIRE.
//
// TLS MATERIAL validation is well covered: absent, unparsable, expired,
// wrongly-chained and mismatched material all fail start, and the front door
// never listens with an identity it cannot prove. Every one of those cells asks
// the question from AUTODB'S SIDE.
//
// What had no cells is the other side: what a real verifying CLIENT does when it
// meets this listener, and what the listener does when a client asks for
// something it must not get. Both halves matter for a different reason — autodb
// refusing bad material of its own is a promise about startup; a client
// rejecting the material autodb DID serve is the promise an operator's
// `sslmode=verify-full` actually depends on.
//
// Driven with Go's crypto/tls as the client. It is one of the three verifiers
// measured in the cross-client reference, and the only one available in-process;
// pgjdbc and libpq behaviour is documented there rather than re-derived here.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// wireBudget bounds every blocking wire operation in this file.
//
// One constant rather than a literal at each site, because the sites that
// LACKED a bound were the defect and a named budget makes a missing one
// visible.
const wireBudget = 5 * time.Second

// sslRequestOffered dials addr, asks for TLS, and returns the connection with
// the 'S' answer already consumed.
//
// EVERY BLOCKING READ HERE IS BOUNDED, which is the reason this helper exists.
// net.DialTimeout bounds only the DIAL. The single byte that follows had no
// deadline in three of this file's paths, so a listener that ACCEPTED and then
// never answered blocked until Go's package-wide test timeout -- taking the
// whole frontdoor package down with it instead of failing one cell. The
// cleartext cell below always set a read deadline; these three did not, and
// the inconsistency inside one file is what made it easy to miss.
//
// The deadline is cleared before returning. That is deliberate rather than an
// oversight: the caller's handshake carries its own bounded context, and
// tls.HandshakeContext closes the connection when that context is done, so the
// handshake is bounded too. Leaving a deadline armed would instead let a
// timeout masquerade as a certificate or version refusal in cells whose whole
// job is to attribute a failure to the right subject.
func sslRequestOffered(t testing.TB, addr string) net.Conn {
	t.Helper()
	raw, err := offerSSLRequest(addr)
	if err != nil {
		t.Fatalf("reaching a TLS listener at %s: %v", addr, err)
	}
	return raw
}

// offerSSLRequest is sslRequestOffered's policy, returning an error instead of
// failing a cell.
//
// Split out so the BOUND ITSELF is drivable. testing.TB is sealed -- it cannot
// be implemented outside the testing package -- so a cell has no way to observe
// a t.Fatalf and carry on. Without this seam, "a silent listener fails fast
// rather than wedging the package" could only be asserted by a hand-rolled copy
// of the code under test, which is a measurement of the copy.
func offerSSLRequest(addr string) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", addr, wireBudget)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	// The bound, covering the write AND the answer.
	if err := raw.SetDeadline(time.Now().Add(wireBudget)); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := raw.Write(sslRequest()); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("write SSLRequest: %w", err)
	}
	answer := make([]byte, 1)
	if _, err := raw.Read(answer); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("read SSLRequest answer: %w", err)
	}
	if answer[0] != 'S' {
		_ = raw.Close()
		return nil, fmt.Errorf("SSLRequest answered %q, want 'S': this listener is not "+
			"offering TLS, so nothing that follows measures TLS", answer[0])
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("clear deadline: %w", err)
	}
	return raw, nil
}

// verifyingClient dials addr, speaks SSLRequest, and then completes a TLS
// handshake WITH VERIFICATION against the given roots and server name.
//
// It returns the handshake error, which is the whole point: these cells are
// about what a client that actually checks decides.
//
// caFile == "" means THE SYSTEM ROOTS, and that is expressed as a nil RootCAs
// because a nil pool is the only way to ask crypto/tls for them. An earlier
// version built x509.NewCertPool() unconditionally and described the empty
// result as "system roots only" in a comment. It was not: an empty pool trusts
// NOTHING, so a client built that way rejects every certificate on earth. Any
// cell relying on it was guaranteed to see a refusal no matter what the
// listener served -- a comment asserting a relation that the code did not
// implement, and the reason the untrusted-CA cell now carries a same-listener
// positive control.
func verifyingClient(t testing.TB, addr, serverName, caFile string) error {
	t.Helper()
	raw := sslRequestOffered(t, addr)
	defer func() { _ = raw.Close() }()

	var roots *x509.CertPool
	if caFile != "" {
		pem, rerr := os.ReadFile(caFile)
		if rerr != nil {
			t.Fatal(rerr)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("the test CA did not parse")
		}
	}
	c := tls.Client(raw, &tls.Config{ServerName: serverName, RootCAs: roots})
	ctx, cancel := context.WithTimeout(context.Background(), wireBudget)
	defer cancel()
	return c.HandshakeContext(ctx)
}

// 1. A NAME THE CERTIFICATE DOES NOT CARRY IS REJECTED BY THE CLIENT.
//
// autodb validates SAN coverage of its OWN configured names at startup, which
// says nothing about a client dialling by some other name. This is the
// `sslmode=verify-full` failure an operator actually hits.
func TestNegativeWire_ClientRejectsAHostnameMismatch(t *testing.T) {
	t.Parallel()
	_, _, addr, ca := listenerWithCA(t, Options{})

	// The listener's material covers autodb.example.com (listenerWith's
	// fixture). Dial claiming to be somebody else.
	err := verifyingClient(t, addr, "not-this-host.example", ca)
	if err == nil {
		t.Fatal("a verifying client accepted a certificate that does not cover the name " +
			"it dialled — sslmode=verify-full would be worthless")
	}
	var nameErr x509.HostnameError
	if !errors.As(err, &nameErr) {
		t.Errorf("the client refused for a reason OTHER than the name, so this cell does "+
			"not measure name verification: %v", err)
	}

	// THE POSITIVE CONTROL, and it is the reason this cell means anything: the
	// SAME client, dialling the name the certificate does carry, succeeds. A
	// listener serving unusable material would fail both.
	if err := verifyingClient(t, addr, "autodb.example.com", ca); err != nil {
		t.Fatalf("the correct name was also rejected, so the failure above says nothing "+
			"about the name: %v", err)
	}
}

// 2. A PRIVATE CA THE CLIENT DOES NOT TRUST IS REJECTED.
//
// The operator-facing half of this is the CA certificate the front door hands
// out (SPC k). If a client without it accepted the connection anyway, that file
// would be decoration.
func TestNegativeWire_ClientRejectsAnUntrustedCA(t *testing.T) {
	t.Parallel()
	_, _, addr, ca := listenerWithCA(t, Options{})

	// System roots only -- nil RootCAs, not an empty pool. The private CA is
	// not among them.
	err := verifyingClient(t, addr, "autodb.example.com", "")
	if err == nil {
		t.Fatal("a verifying client accepted a private CA it had never been given")
	}
	var authErr x509.UnknownAuthorityError
	if !errors.As(err, &authErr) {
		t.Errorf("the refusal is not an unknown-authority failure, so this cell does not "+
			"measure trust: %v", err)
	}

	// THE POSITIVE CONTROL, ON THE SAME LISTENER, and without it the assertion
	// above is not about trust at all.
	//
	// Review found the original arrangement: the client was built with an
	// EMPTY certificate pool, which trusts nothing, so it would have rejected
	// this listener whatever it served -- unusable material, a self-signed
	// stub, anything. "Rejected" proved only that a refusal happened.
	//
	// Handing the SAME client the CA the front door actually hands out (SPC k)
	// must SUCCEED against the SAME listener. That is what makes the refusal
	// above attributable to the missing trust anchor, and it is what makes the
	// distributed CA file load-bearing rather than decoration.
	if err := verifyingClient(t, addr, "autodb.example.com", ca); err != nil {
		t.Fatalf("the served CA was ALSO rejected, so the refusal above says nothing "+
			"about trust — this listener is not serving verifiable material: %v", err)
	}
}

// 3. A DOWNGRADE IS REFUSED. TLS 1.0 and 1.1 are broken in public; the floor is
// a minimum, not a target.
func TestNegativeWire_ObsoleteTLSVersionsAreRefused(t *testing.T) {
	t.Parallel()
	_, _, addr := listenerWith(t, Options{})

	for name, max := range map[string]uint16{
		"TLS1.0": tls.VersionTLS10,
		"TLS1.1": tls.VersionTLS11,
	} {
		t.Run(name, func(t *testing.T) {
			raw := sslRequestOffered(t, addr)
			defer func() { _ = raw.Close() }()
			// BOTH bounds pinned to the obsolete version, and Min is the one
			// that matters. With only MaxVersion set, Go's own client floor
			// (TLS 1.2 since Go 1.22) refuses the handshake before a byte
			// reaches autodb — so the cell passed while measuring the CLIENT.
			// Proven: lowering the listener's MinVersion to TLS 1.0 left it
			// green. Setting MinVersion here makes the client willing to speak
			// the old version, so the refusal that fails the handshake is
			// autodb's.
			//
			// InsecureSkipVerify because the VERSION is the subject —
			// verification is cells 1 and 2, and leaving it on would let a
			// certificate error masquerade as a version refusal.
			c := tls.Client(raw, &tls.Config{
				ServerName:         "autodb.example.com",
				InsecureSkipVerify: true, //nolint:gosec // the version is the subject
				MinVersion:         max,
				MaxVersion:         max,
			})
			ctx, cancel := context.WithTimeout(context.Background(), wireBudget)
			defer cancel()
			if err := c.HandshakeContext(ctx); err == nil {
				t.Errorf("%s completed a handshake; the floor is TLS 1.2", name)
			}
		})
	}

	// POSITIVE CONTROL: 1.2 IS accepted, so the refusals above are about the
	// obsolete versions rather than about the harness.
	raw := sslRequestOffered(t, addr)
	defer func() { _ = raw.Close() }()
	c := tls.Client(raw, &tls.Config{
		ServerName:         "autodb.example.com",
		InsecureSkipVerify: true, //nolint:gosec // the version is the subject
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), wireBudget)
	defer cancel()
	if err := c.HandshakeContext(ctx); err != nil {
		t.Errorf("TLS 1.2 was refused, so the version cells above prove nothing: %v", err)
	}
}

// 4. THE LISTENER NEVER ASKS FOR A CLIENT CERTIFICATE.
//
// Stated as a cell because the four-gaps register asked for "a client
// presenting no certificate where one is required" — and MEASURED, there is no
// such configuration: autodb authenticates with PATs and passwords, and nothing
// in the front door sets ClientAuth. So the honest cell is the opposite one, and
// it is worth having: switching client auth on would refuse every existing
// client at the handshake, before any autodb code could explain why.
func TestNegativeWire_NoClientCertificateIsEverRequested(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ch := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	cfg, err := LoadServerTLS(fdWith(ch.bundle, ch.key, ch.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert: autodb authenticates with tokens, "+
			"and requesting a certificate would refuse every client at the handshake — "+
			"before any autodb code could say why", cfg.ClientAuth)
	}
	if cfg.ClientCAs != nil {
		t.Error("a client CA pool is configured for a listener that does not verify clients")
	}

	// And a client offering NO certificate connects normally, which is the
	// behaviour the absence is for.
	_, _, addr, ca := listenerWithCA(t, Options{})
	if err := verifyingClient(t, addr, "autodb.example.com", ca); err != nil {
		t.Errorf("a client with no certificate was refused: %v", err)
	}
}

// 5. SSLRequest ON A CLEARTEXT LISTENER IS DECLINED WITH 'N', AND THE CLIENT
// CARRIES ON.
//
// The uncovered case: the SSLRequest -> 'S' path on a TLS listener has a cell,
// and GSS -> 'N' has one, but nothing drove SSLRequest against the cleartext
// debugging transport. It matters because a client asking for TLS there is NOT
// an error — libpq's sslmode=prefer asks every time — so the listener must
// decline the option and keep reading the startup that follows. Answering 'S'
// would begin a TLS handshake this listener cannot complete; treating it as
// malformed would refuse every prefer-mode client.
//
// A cleartext listener also must never come into being by ACCIDENT, which is
// the second half of the cell: a nil tls.Config alone is refused, so the day a
// certificate fails to load this surface does not quietly start serving in the
// clear.
func TestNegativeWire_CleartextListenerDeclinesSSLAndKeepsReading(t *testing.T) {
	t.Parallel()

	// A nil tls.Config WITHOUT the explicit acknowledgement is refused. The
	// positive control for the whole cell: it proves cleartext is reachable
	// only deliberately, so what follows is testing an opt-in and not an
	// accident.
	if _, err := Open("127.0.0.1:0", nil, Options{}); err == nil {
		t.Fatal("a listener opened with no TLS material and no explicit cleartext " +
			"acknowledgement; a failed certificate load would serve in the clear")
	}

	l, err := Open("127.0.0.1:0", nil, Options{
		CleartextDebug:    true,
		MaxConns:          unthrottled,
		AuthFailuresPerIP: unthrottled,
	})
	if err != nil {
		t.Fatalf("Open(cleartext): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); l.Close() })

	raw, err := net.DialTimeout("tcp", l.Addr().String(), wireBudget)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	if _, err := raw.Write(sslRequest()); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	answer := make([]byte, 1)
	if err := raw.SetReadDeadline(time.Now().Add(wireBudget)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Read(answer); err != nil {
		t.Fatalf("no answer to SSLRequest on a cleartext listener: %v", err)
	}
	if answer[0] != 'N' {
		t.Fatalf("SSLRequest answered %q on a CLEARTEXT listener, want 'N' — %q would "+
			"begin a handshake this listener cannot complete", answer[0], answer[0])
	}

	// AND IT KEPT READING. 'N' declines the option; it does not end the
	// connection. A listener that closed here would refuse every
	// sslmode=prefer client, which asks for TLS on every connection.
	if _, err := raw.Write(startupPacket(196608, map[string]string{"user": "nobody", "database": "nothing"})); err != nil {
		t.Fatalf("the connection did not survive the declined SSLRequest: %v", err)
	}
	if err := raw.SetReadDeadline(time.Now().Add(wireBudget)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 1)
	if _, err := raw.Read(reply); err != nil {
		t.Fatalf("the startup that followed a declined SSLRequest got no answer at all, "+
			"so the listener stopped reading after 'N': %v", err)
	}
}

// 6. A LISTENER THAT ACCEPTS AND THEN SAYS NOTHING FAILS FAST.
//
// Review found this one, and it is a property of the CELLS rather than of
// autodb: net.DialTimeout bounds only the dial, so the single SSLRequest answer
// byte was read with no deadline in three of this file's paths. A listener that
// completed the TCP accept and then never wrote left that read blocking until
// Go's package-wide test timeout -- which does not fail one cell, it takes the
// whole frontdoor package down and reports no useful subject.
//
// The cleartext cell above always set a read deadline. Three others did not,
// and one file holding both conventions is why it went unnoticed.
//
// Driven against a bare net.Listener that accepts and writes nothing, which is
// exactly the shape described. The assertion is on the ERROR and the ELAPSED
// TIME: a bound that is merely present but never reached would satisfy the
// first and not the second.
func TestNegativeWire_AnAcceptingSilentListenerIsBounded(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Accept and hold. The connection must stay OPEN and silent: closing it
	// would produce an immediate EOF and the cell would pass without the
	// deadline ever mattering.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		accepted <- c
	}()

	start := time.Now()
	conn, err := offerSSLRequest(ln.Addr().String())
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	default:
	}

	if err == nil {
		t.Fatal("a silent listener produced a usable connection")
	}
	// It must be a TIMEOUT. Any other error would mean the cell measured
	// something else -- a refused dial, an EOF from a closed peer -- and the
	// bound would still be unproven.
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Errorf("error is not a timeout, so the deadline is not what stopped this: %v", err)
	}
	// And it stopped at the budget rather than at Go's test timeout. The slack
	// is for scheduling, not for a second bound.
	if elapsed > wireBudget+2*time.Second {
		t.Errorf("took %v to give up against a budget of %v: the read is not bounded by "+
			"the deadline this helper sets", elapsed, wireBudget)
	}
}

// listenerServingAsOf opens a listener on material loaded AS OF loadedAt,
// rather than as of now.
//
// The seam that makes the validity cells possible, and it is not a cheat.
// LoadServerTLS checks notBefore/notAfter against the time it is given, so
// passing a moment when the material WAS valid reproduces exactly the
// production situation the startup check cannot cover: a daemon that started
// with good material and is still running when the clock leaves its window.
func listenerServingAsOf(t testing.TB, c chain, loadedAt time.Time) string {
	t.Helper()
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), loadedAt)
	if err != nil {
		t.Fatalf("material that should load as of %s did not: %v", loadedAt, err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{
		MaxConns: unthrottled, AuthFailuresPerIP: unthrottled,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); l.Close() })
	return l.Addr().String()
}

// 7. AN EXPIRED OR NOT-YET-VALID CERTIFICATE, AS THE CLIENT SEES IT.
//
// The last item the parent register named and the only one that had no cell.
// Review was right that it was neither added nor retired, and it should be
// added rather than retired: LoadServerTLS refuses to START on material outside
// its validity window, but that invariant is evaluated ONCE. A front door
// started with a 24-hour certificate is still serving it in hour 25, and a host
// whose clock is behind serves material that has not begun. Neither is
// reachable through the startup path, and both are ordinary operational
// reality — a missed rotation and clock skew.
//
// So the listener here is loaded as of a moment when its material was valid,
// and dialled by a client living now. The valid case is built by the SAME
// helper and is the positive control: without it, three rejections would be
// consistent with a listener that cannot serve anything.
func TestNegativeWire_ClientRejectsMaterialOutsideItsValidityWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()

	for _, tc := range []struct {
		name                string
		notBefore, notAfter time.Time
		loadedAt            time.Time
		wantReject          bool
		// detail is the substring x509 uses to say WHICH end of the window was
		// missed. Go reports both as x509.Expired, so the reason alone cannot
		// tell an expired certificate from one that has not begun.
		detail string
	}{
		{
			// Rotation missed: valid when the daemon started, expired since.
			name:      "expired while the listener was running",
			notBefore: now.Add(-2 * time.Hour), notAfter: now.Add(-time.Hour),
			loadedAt: now.Add(-90 * time.Minute), wantReject: true,
			detail: "is after",
		},
		{
			// Clock skew: the material's window has not opened yet.
			name:      "not yet valid",
			notBefore: now.Add(time.Hour), notAfter: now.Add(2 * time.Hour),
			loadedAt: now.Add(90 * time.Minute), wantReject: true,
			detail: "is before",
		},
		{
			// THE POSITIVE CONTROL, built by the same helper with the same CA.
			name:      "inside its window",
			notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour),
			loadedAt: now, wantReject: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := issueChain(t, []string{"autodb.example.com"}, tc.notBefore, tc.notAfter)
			addr := listenerServingAsOf(t, c, tc.loadedAt)

			// Verified against the CA this listener actually serves, so the
			// only thing left to fail on is the validity window.
			err := verifyingClient(t, addr, "autodb.example.com", c.ca)

			if !tc.wantReject {
				if err != nil {
					t.Fatalf("material inside its window was rejected, so the refusals in "+
						"the other cases say nothing about validity: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a verifying client accepted material outside its validity window")
			}
			// BY VALUE: crypto/x509 returns CertificateInvalidError as a value,
			// and a pointer target here would fall through and match nothing.
			var invalid x509.CertificateInvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("the refusal is not a validity failure, so this cell does not "+
					"measure the window: %v", err)
			}
			if invalid.Reason != x509.Expired {
				t.Errorf("reason = %v, want x509.Expired", invalid.Reason)
			}
			// And the right END of the window, which the reason cannot say.
			if !strings.Contains(invalid.Detail, tc.detail) {
				t.Errorf("detail %q does not contain %q: the client refused for a validity "+
					"reason, but not the one this case constructs", invalid.Detail, tc.detail)
			}
		})
	}
}
