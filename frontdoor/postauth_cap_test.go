package frontdoor

import (
	"encoding/binary"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// OVER THE POST-AUTH CAP → 08P01, AND THE BODY IS NOT DECODED — MEASURED.
//
// The wire cannot tell you the second half. A refusal looks IDENTICAL whether
// the body was read first or never touched: same 08P01, same close, same bytes
// in the same order. So a cell that receives the ErrorResponse and stops has
// asserted the refusal in prose and the resource property not at all — lector's
// r2 finding, and he is right that a regression which releases the body before
// sending the same fatal response would stay green under it.
//
// What makes the property observable is the reader's own delivery counter,
// reached through a per-session test hook, because the reader is per-connection
// and owned by the session goroutine. THE MEASUREMENT IS THE ASSERTION.
//
// The measured answer is stronger than the one asked for: not five header bytes
// but ZERO. The reader frames the header itself, byte at a time, and the loop
// refuses from the DECLARED LENGTH before admitting anything — so pgproto3 is
// never handed the header either, let alone the body.
//
// Driven at a LOWERED cap so this costs milliseconds instead of moving 64 MiB.
// That is only legitimate because the real figure is pinned separately by
// TestPostAuth_TheCapIsTheDocumentedSixtyFourMiB — a lowered-cap cell proves the
// MECHANISM and would quietly stand in for the CONTRACT if it stood alone.
func TestPostAuth_AFrameOverTheCapIsRefusedWithoutDecodingItsBody(t *testing.T) {
	t.Parallel()
	const small = 4096

	// The hook hands over the per-session reader the moment it exists.
	var mu sync.Mutex
	var readers []*frameReader
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
		MaxBodyBytes:    small,
		testReaderReady: func(fr *frameReader) { mu.Lock(); readers = append(readers, fr); mu.Unlock() },
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()

	mu.Lock()
	if len(readers) == 0 {
		mu.Unlock()
		t.Fatal("no session reader was published — the hook is the instrument, and a cell that " +
			"cannot read the instrument measures nothing")
	}
	fr := readers[len(readers)-1]
	mu.Unlock()

	// The baseline is taken AFTER authentication, because the credential exchange
	// legitimately delivers its own frames through this same reader. What this
	// cell owns is the delta across ONE over-cap frame.
	before := fr.deliveredBytes()

	// THE HEADER, AND ONLY A PREFIX OF THE BODY.
	//
	// This is not a convenience. Sending the whole frame races the server's own
	// correctness: it refuses from the declared length and closes, so on a loaded
	// runner the client's remaining bytes hit a closed socket and the cell fails
	// on a broken pipe while the front door is behaving exactly as designed.
	// (That is precisely what CI caught and two green local machines did not.)
	//
	// Writing the header plus a prefix asserts the stronger property anyway: the
	// refusal is decided WITHOUT the body existing on the wire at all, so it
	// cannot have waited for it, and the prefix that was sent is measured as
	// never reaching pgproto3.
	const bodyLen = 16400 // comfortably past the 4096-byte cap
	frame := []byte{'Q'}
	frame = binary.BigEndian.AppendUint32(frame, uint32(4+bodyLen))
	frame = append(frame, []byte(strings.Repeat("y", 100))...)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("writing a %d-byte header declaring %d body bytes: %v", len(frame), bodyLen, err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("a frame over the cap must be REFUSED with a frame, not by silence: %v — §7 gives it "+
			"08P01 precisely so a client is not left reading a dead socket", err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("frame = %T, want ErrorResponse; a declared %d-byte body passed a %d-byte cap", msg, bodyLen, small)
	}
	if e.Code != sqlStateProtocolViolation || e.Detail != ruleMessageTooLarge {
		t.Fatalf("refusal = %s/%s, want %s/%s — the matrix gives an over-cap frame 08P01 because a "+
			"stream we refuse to read cannot be resynchronised",
			e.Code, e.Detail, sqlStateProtocolViolation, ruleMessageTooLarge)
	}

	// THE HALF THE WIRE CANNOT SHOW. The frame declares 5 header bytes plus a
	// bodyLen-byte body; if any of it reached pgproto3 the refusal cost what it
	// was refusing, which is the whole point of refusing from the header.
	if got := fr.deliveredBytes() - before; got != 0 {
		t.Errorf("%d bytes of a refused frame reached the Backend, want 0 — the refusal is made "+
			"from the DECLARED LENGTH before admission, so neither header nor body may be delivered; a "+
			"non-zero delta means the body was released and then refused, which is invisible on the wire", got)
	}
}
