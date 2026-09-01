package frontdoor

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// F4 frame conformance and fuzzing for the PRE-AUTH startup exchange.
//
// The per-slice tests in startup_test.go prove individual matrix rows behave
// as specified. This file asks a different question, which is F4's question:
// does the frame layer hold up against inputs nobody designed for, and do the
// two places that implement the length rule still agree?
//
// Scope is deliberately pre-auth only. Nothing here needs a credential store,
// a session, or a query path, so it runs today against F0 and does not wait
// on F1/F2.

// ---------------------------------------------------------------------------
// 1. The length rule has TWO implementations. They must not drift.
// ---------------------------------------------------------------------------

// classifyStartupLength decides from the declared length using int64
// arithmetic; readStartupPacket applies the same bound again using int
// arithmetic on its own read path. Two copies of one rule is a drift hazard
// of exactly the kind that stays invisible until the copies disagree on a
// value nobody tried -- so this pins them to each other across the ENTIRE
// uint32 domain rather than at a few hand-picked points.
//
// The int-vs-int64 difference is the specific reason this is worth pinning:
// on a 32-bit platform int(uint32) is negative for anything above MaxInt32,
// so the two expressions do not compute the same intermediate value. They
// agree on the VERDICT today; nothing in the source makes them keep agreeing.

// boundOfReadStartupPacket reports whether readStartupPacket would reject a
// frame declaring this length, exercising the real function rather than a
// restatement of its rule. A restatement would drift with the thing it is
// supposed to be pinning, which would make this whole cell decorative.
func boundOfReadStartupPacket(declared uint32) (rejected bool) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], declared)
	// A reader that supplies the header and then never enough body. A frame
	// inside the bound fails with io.ErrUnexpectedEOF (accepted, then starved);
	// one outside fails with the bounds error before any allocation.
	_, err := readStartupPacket(io.MultiReader(
		bytesReader(hdr[:]),
		starvedReader{},
	))
	return err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF)
}

type starvedReader struct{}

func (starvedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestStartupLength_BoundaryTable(t *testing.T) {
	// The boundaries are where a length rule is wrong, so they are named
	// explicitly rather than left for the fuzzer to stumble onto. declared is
	// the wire value; the body is declared-4.
	cases := []struct {
		name     string
		declared uint32
		reject   bool
	}{
		{"zero", 0, true},
		{"three, shorter than the header itself", 3, true},
		{"four, header only, body 0", 4, true},
		{"seven, body 3, one below the minimum", 7, true},
		{"eight, body 4, the smallest legal body", 8, false},
		{"nine, body 5", 9, false},
		{"cap exactly", PreAuthMaxBodyLen + 4, false},
		{"one past the cap", PreAuthMaxBodyLen + 5, true},
		{"max uint32, the overflow probe", ^uint32(0), true},
		{"max int32 boundary", 1 << 31, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, bad := classifyStartupLength(c.declared)
			if bad != c.reject {
				t.Fatalf("classifyStartupLength(%d) rejected=%v, want %v", c.declared, bad, c.reject)
			}
			if got := boundOfReadStartupPacket(c.declared); got != c.reject {
				t.Fatalf("readStartupPacket bound for %d rejected=%v, want %v (the two copies disagree)",
					c.declared, got, c.reject)
			}
		})
	}
}

func TestStartupLength_ReasonsAreDistinguishable(t *testing.T) {
	// F0b's lesson, kept as a regression: an under-length frame was once
	// audited as pre-auth-message-too-large, pointing an operator the exact
	// opposite way. The reasons must stay distinct AND correctly assigned.
	short, bad := classifyStartupLength(4) // body 0
	if !bad || short != reasonStartupMalformed.String() {
		t.Fatalf("under-length frame classified %q (bad=%v), want %q",
			short, bad, reasonStartupMalformed.String())
	}
	over, bad := classifyStartupLength(PreAuthMaxBodyLen + 5)
	if !bad || over != reasonPreAuthOversize.String() {
		t.Fatalf("over-cap frame classified %q (bad=%v), want %q",
			over, bad, reasonPreAuthOversize.String())
	}
	if short == over {
		t.Fatal("the two failure reasons are indistinguishable")
	}
}

// FuzzStartupLength pins the two implementations across the whole domain.
//
// Property: for every uint32, classifyStartupLength and readStartupPacket's
// own bound reach the SAME accept/reject verdict. The fuzzer is looking for
// the value where the int64 and int expressions part company.
func FuzzStartupLength(f *testing.F) {
	for _, seed := range []uint32{
		0, 3, 4, 7, 8, 9,
		PreAuthMaxBodyLen + 3, PreAuthMaxBodyLen + 4, PreAuthMaxBodyLen + 5,
		1 << 31, ^uint32(0), ^uint32(0) - 4,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, declared uint32) {
		_, classifierRejects := classifyStartupLength(declared)
		readerRejects := boundOfReadStartupPacket(declared)
		if classifierRejects != readerRejects {
			t.Fatalf("length %d: classifier rejects=%v but reader rejects=%v — "+
				"the two copies of the pre-auth length rule have drifted",
				declared, classifierRejects, readerRejects)
		}
		if classifierRejects {
			reason, _ := classifyStartupLength(declared)
			if reason == "" {
				t.Fatalf("length %d rejected with an empty reason: a refusal "+
					"nobody can name is a refusal nobody can audit", declared)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 2. The socket must survive arbitrary bytes.
// ---------------------------------------------------------------------------

// FuzzStartupFrame drives the REAL listener with arbitrary opening bytes.
//
// Three invariants, none of which depend on what the bytes mean:
//   - the listener never panics (the fuzz target crashes the process if it does)
//   - every connection terminates well inside the pre-auth deadline; a peer
//     must not be able to hold a socket open for free by sending nonsense
//   - the goroutine count returns to its baseline, so a malformed frame does
//     not leak the goroutine that was serving it
func FuzzStartupFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x01})                   // TLS ClientHello signature
	f.Add(sslRequest())                                           // the legitimate opener
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}) // GSSENCRequest
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})                         // max declared length
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})                         // zero length
	f.Add([]byte{0x00, 0x00, 0x00, 0x08})                         // header promising a body that never comes

	_, _, addr := liveListener(f)

	f.Fuzz(func(t *testing.T, opening []byte) {
		before := runtime.NumGoroutine()

		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Skipf("dial: %v", err) // the listener is shared; a refused dial is not the subject
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = conn.Write(opening)
			// Drain until the peer hangs up. What comes back does not matter
			// here -- that the socket ENDS does.
			_, _ = io.Copy(io.Discard, conn)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = conn.Close()
			t.Fatalf("connection did not terminate within 5s for opening %x — "+
				"a peer can hold a pre-auth socket open with arbitrary bytes", opening)
		}
		_ = conn.Close()

		// Goroutines unwind asynchronously; give the server a bounded moment
		// rather than asserting on an instant that has no reason to be settled.
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before+4 {
			t.Fatalf("goroutines %d -> %d for opening %x: a malformed frame leaked its server goroutine",
				before, after, opening)
		}
	})

}

// TestStartupFrame_HeaderWithoutBodyIsBoundedByTheDeadline records what a
// declared-but-unsent body actually costs an attacker today.
//
// MEASURED, not assumed: a 4-byte header naming a legal body length and then
// silence holds the pre-auth slot for the FULL StartupDeadline, and the server
// sends no denial frame at all -- the client's read times out first. My first
// version of this cell asserted a prompt denial and failed at exactly 10.00s,
// which is how the behaviour was found rather than guessed.
//
// That is not a defect against F0: the deadline IS the bound, and it holds.
// It is the slowloris shape F0e is chartered to price (ADR-0075 F0e:
// "slowloris and malformed-frame fuzzing"), so this cell pins the current
// contract -- bounded and terminating -- and will need revisiting when F0e
// lands a tighter budget. What it must never do is pass while the connection
// hangs forever.
func TestStartupFrame_HeaderWithoutBodyIsBoundedByTheDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the pre-auth deadline in real time")
	}
	_, _, addr := liveListener(t)

	conn := dial(t, addr)
	defer conn.Close()

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 8) // legal declared length, body 4
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Generous slack over the deadline: the assertion is TERMINATION, not the
	// precise instant, and a tight bound here would flake on a loaded runner.
	_ = conn.SetReadDeadline(time.Now().Add(StartupDeadline + 5*time.Second))
	start := time.Now()
	_, err := io.Copy(io.Discard, conn)
	elapsed := time.Since(start)

	if elapsed >= StartupDeadline+5*time.Second {
		t.Fatalf("connection still open after %v: an unsent body holds a pre-auth "+
			"slot indefinitely, not merely for the deadline", elapsed)
	}
	// The connection ended. Either a clean close or a reset is a termination;
	// what matters is that the peer did not get to keep the slot.
	_ = err
	if elapsed < StartupDeadline/2 {
		t.Fatalf("connection ended after only %v, well before StartupDeadline (%v) — "+
			"the contract this cell pins has changed and its comment is now wrong",
			elapsed, StartupDeadline)
	}
	t.Logf("unsent body held the pre-auth slot for %v (StartupDeadline=%v, no denial frame sent)",
		elapsed.Round(time.Millisecond), StartupDeadline)
}
