package frontdoor

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Fuzzing the pre-auth surface (ADR-0075's F0 exit criterion).
//
// EVERYTHING here is reachable by anyone with a TCP route and no credential,
// which makes it the part of the system where a panic is not a crash report
// but a denial of service: a nil dereference in the startup parser takes the
// daemon down for every session it is serving, at a cost to the attacker of
// one connection.
//
// The property under test is deliberately weak and absolute: for ANY bytes,
// the connection is handled and released. Not "produces the right denial" —
// the reason is checked by the cells above, and a fuzzer that asserted a
// specific outcome would spend its budget rediscovering cases those already
// pin. What a fuzzer is uniquely good at is finding the input nobody thought
// of, and what it needs to be good at that is a property that holds for all
// of them.

// fuzzTLS builds the server material once. Per-iteration certificate
// generation would spend the entire fuzzing budget on ECDSA.
var (
	fuzzTLSOnce sync.Once
	fuzzTLSCfg  *tls.Config
)

func fuzzTLSConfig(t testing.TB) *tls.Config {
	fuzzTLSOnce.Do(func() {
		now := time.Now()
		c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
		cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
		if err != nil {
			t.Fatalf("fuzz material: %v", err)
		}
		fuzzTLSCfg = cfg
	})
	return fuzzTLSCfg
}

// fuzzDeadlines keep an iteration short. The enforcement path is the real
// one; only the numbers are small.
var fuzzDeadlines = deadlines{
	tls:     50 * time.Millisecond,
	startup: 50 * time.Millisecond,
	auth:    50 * time.Millisecond,
	idle:    50 * time.Millisecond,
}

// FuzzS0 drives arbitrary bytes at the plaintext startup phase.
//
// This is the first thing an anonymous peer touches: the direct-TLS sniffer,
// the length classifier, the GSS loop and the startup decode, none of which
// has authenticated anybody. Two of the three bugs this file's subject has
// already had were classification mistakes on bytes nobody sent deliberately.
func FuzzS0(f *testing.F) {
	cfg := fuzzTLSConfig(f)

	f.Add(sslRequest())
	f.Add(startupPacket(protocolVersion30, map[string]string{"user": "root", "database": "lm"}))
	f.Add(startupPacket(uint32(4)<<16|0, map[string]string{"user": "root"}))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}) // a TLS ClientHello's opening
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})                                     // a length of zero
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})                                     // a length of four billion
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f})             // a GSSENCRequest
	f.Add([]byte{0x00, 0x00, 0x00, 0x10, 0x04, 0xd2, 0x16, 0x2e, 0, 0, 0, 1, 0, 0, 0, 2})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = server.Close() }()
			secure, _, _ := runStartup(server, cfg, time.Now, fuzzDeadlines)
			if secure != nil {
				_ = secure.Close()
			}
		}()

		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		go func() {
			_, _ = client.Write(in)
			_, _ = io.Copy(io.Discard, client)
			_ = client.Close()
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			// A hang is the same outage as a panic, reached differently:
			// a connection the server will not let go of is one an
			// attacker can open by the thousand.
			t.Fatalf("runStartup did not return for %d bytes: %x", len(in), in)
		}
		_ = client.Close()
	})
}

// fuzzListenerAddress starts the listener the wire-level target dials.
//
// Built EAGERLY in the target's body and torn down by Cleanup — never lazily
// from inside a goroutine that outlives it. The first version deferred the
// construction behind a sync.Once whose goroutine then called TB.Context(),
// and Go's testing package panics when a fuzz target's TB methods are used
// from inside the fuzzing loop. It passed every run until a mutation shifted
// the timing, which is the shape of a CI flake nobody can reproduce: the
// harness was the bug, and it would have been blamed on the subject.
func fuzzListenerAddress(t testing.TB) string {
	l, err := Open("127.0.0.1:0", fuzzTLSConfig(t), Options{
		Authn:             &fakeAuth{result: goodSession()},
		AuthFailuresPerIP: 1 << 30,
		MaxConns:          4096,
		PreAuthMaxConns:   4096,
		ControlLaneBytes:  4096 * ControlLanePerConn,
		testDeadlines: &deadlines{
			tls: time.Second, startup: 200 * time.Millisecond,
			auth: 200 * time.Millisecond, idle: 200 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("fuzz listener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); l.Close() })
	return l.Addr().String()
}

// FuzzWireAfterStartup drives arbitrary bytes at the credential exchange over
// a REAL TLS connection that has completed a real startup.
//
// The startup is driven properly and only the bytes AFTER it are arbitrary,
// so the budget is spent past the point FuzzS0 already covers — a fuzzer that
// had to guess its way through a TLS handshake would never reach this code at
// all.
func FuzzWireAfterStartup(f *testing.F) {
	addr := fuzzListenerAddress(f)

	f.Add([]byte{'p', 0, 0, 0, 5, 0})                     // an empty password
	f.Add([]byte{'p', 0, 0, 0, 9, 'h', 'u', 'n', 't', 0}) // an ordinary one
	f.Add([]byte{'Q', 0, 0, 0, 6, ';', 0})                // a query before auth
	f.Add([]byte{'X', 0, 0, 0, 4})                        // Terminate before auth
	f.Add([]byte{'p', 0xff, 0xff, 0xff, 0xff})            // a body of four billion
	f.Add([]byte{'p', 0, 0, 0, 3})                        // a body shorter than its header
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00})           // a message type of zero
	f.Add([]byte{'p', 0, 1, 0, 5, 0})                     // a body of 65 KiB, past the pre-auth cap
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Skipf("dial: %v", err)
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

		if _, werr := raw.Write(sslRequest()); werr != nil {
			return
		}
		answer := make([]byte, 1)
		if _, rerr := raw.Read(answer); rerr != nil || answer[0] != 'S' {
			return
		}
		tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the client is not the subject
		if herr := tc.Handshake(); herr != nil {
			return
		}
		if _, werr := tc.Write(startupPacket(protocolVersion30, defaultParams())); werr != nil {
			return
		}
		// The server's next word is the auth request; read it and then say
		// whatever the fuzzer decided.
		head := make([]byte, 9)
		if _, rerr := io.ReadFull(tc, head); rerr != nil {
			return
		}
		_, _ = tc.Write(in)

		// The connection must END. Not with any particular answer — with an
		// end. A peer that can make this loop forever needs no credential to
		// take the listener down.
		_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, rerr := io.Copy(io.Discard, tc); rerr != nil {
			var ne net.Error
			if ok := asNetError(rerr, &ne); ok && ne.Timeout() {
				t.Fatalf("the connection was still open five seconds after %d bytes: %x", len(in), in)
			}
		}
	})
}

func asNetError(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok { //nolint:errorlint // the concrete wrap is what net returns here
		*target = ne
		return true
	}
	return false
}

// FuzzClassifyStartupLength is the pure half: the classifier that decides,
// from four bytes and nothing else, whether a frame is malformed, oversize or
// worth reading.
//
// It got this wrong twice — an under-length frame audited as too large, and
// an over-cap frame audited as a PostgreSQL 17 direct-TLS attempt — both
// times by inferring a cause from a symptom several causes share. A pure
// function with a total contract is exactly what a fuzzer settles.
func FuzzClassifyStartupLength(f *testing.F) {
	for _, seed := range []uint32{0, 1, 4, 8, 9, 296, PreAuthMaxBodyLen + 4, PreAuthMaxBodyLen + 5, 0x16030100, ^uint32(0)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, declared uint32) {
		reason, bad := classifyStartupLength(declared)
		body := int64(declared) - 4
		switch {
		case body < startupMinBody:
			if !bad || reason != reasonStartupMalformed.String() {
				t.Fatalf("declared %d (body %d) classified %q/%v, want malformed", declared, body, reason, bad)
			}
		case body > PreAuthMaxBodyLen:
			if !bad || reason != reasonPreAuthOversize.String() {
				t.Fatalf("declared %d (body %d) classified %q/%v, want oversize", declared, body, reason, bad)
			}
		default:
			if bad {
				t.Fatalf("declared %d (body %d) was refused as %q; it is in bounds", declared, body, reason)
			}
		}
	})
}

// FuzzReadStartupPacket pins the allocation guard.
//
// The bound is applied to the LENGTH FIELD before anything is allocated,
// which is the whole reason the pre-auth cap is a separate and much smaller
// number: a peer's first act must not be to name a size we then reserve for
// them.
func FuzzReadStartupPacket(f *testing.F) {
	f.Add(sslRequest())
	f.Add([]byte{0, 0, 0, 8, 0, 3, 0, 0})
	f.Add([]byte{0x7f, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 4})
	f.Fuzz(func(t *testing.T, in []byte) {
		body, err := readStartupPacket(newLimitedReader(in))
		if err != nil {
			return
		}
		if len(body) > PreAuthMaxBodyLen {
			t.Fatalf("read a %d-byte body past the %d-byte pre-auth cap", len(body), PreAuthMaxBodyLen)
		}
		if len(body) < startupMinBody {
			t.Fatalf("accepted a %d-byte body; nothing that short is a startup packet", len(body))
		}
		declared := binary.BigEndian.Uint32(in[:4])
		if int64(len(body)) != int64(declared)-4 {
			t.Fatalf("declared %d, read %d", int64(declared)-4, len(body))
		}
	})
}

func newLimitedReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
