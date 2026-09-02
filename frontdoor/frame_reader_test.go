package frontdoor

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The framing state machine, tested directly rather than only through a loop.
//
// The defect it exists to prevent is not hypothetical: the first version reset
// lenHave only when the NEXT type byte arrived, so every completed message left
// the reader looking mid-message and the first idle session after any message
// was reported as a frame stall. That reached a green build and was caught by
// the idle-expiry cell — indirectly, by its consequence. These cells assert the
// machine itself, so the next such slip fails where it happens.
func TestFrameReader(t *testing.T) {
	// Message helpers: type byte, then a length that COUNTS ITSELF, then body.
	msg := func(typ byte, body string) []byte {
		out := []byte{typ, 0, 0, 0, 0}
		n := len(body) + 4
		out[1] = byte(n >> 24)
		out[2] = byte(n >> 16)
		out[3] = byte(n >> 8)
		out[4] = byte(n)
		return append(out, body...)
	}

	t.Run("a completed message leaves the reader between messages", func(t *testing.T) {
		fr := newFrameReader(bytes.NewReader(msg('Q', "SELECT 1")))
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("reading a well-formed Query: %v", err)
		}
		if fr.midMessage() {
			t.Fatal("the message is complete, but the reader still reports the peer mid-frame — " +
				"an idle session would be audited as a frame stall and send an operator " +
				"chasing a slow client that was simply not asking for anything")
		}
	})

	t.Run("a type byte inside a body is body, not framing", func(t *testing.T) {
		// The SQL text contains bytes that are valid message types ('Q', 'X',
		// 'p'). A reader that searched the stream rather than tracking
		// boundaries would frame on them.
		fr := newFrameReader(bytes.NewReader(msg('Q', "SELECT 'QXp' AS s")))
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("a body containing type-like bytes: %v", err)
		}
		if fr.midMessage() {
			t.Fatal("body content was mistaken for framing")
		}
	})

	t.Run("an empty-body message frames correctly", func(t *testing.T) {
		// Sync, Flush and Terminate are four bytes: the length field and nothing
		// else. An off-by-one that expects a body would strand the next message.
		fr := newFrameReader(bytes.NewReader(append(msg('S', ""), msg('Q', "SELECT 1")...)))
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("Sync followed by Query: %v", err)
		}
		if fr.midMessage() {
			t.Fatal("an empty-body message left the reader mid-frame")
		}
	})

	t.Run("every message start is reported, including pipelined ones", func(t *testing.T) {
		// The whole point: three messages arriving in ONE read must produce
		// three starts, because each one is a fresh chance for the peer to stall.
		starts := 0
		fr := newFrameReader(bytes.NewReader(bytes.Join([][]byte{
			msg('Q', "SELECT 1"), msg('Q', "SELECT 2"), msg('S', ""),
		}, nil)))
		fr.setOnStart(func() { starts++ })
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("three pipelined messages: %v", err)
		}
		if starts != 3 {
			t.Fatalf("message starts = %d, want 3 — a start that is not reported is a "+
				"message with no stall budget", starts)
		}
	})

	t.Run("a message split across reads is still one message", func(t *testing.T) {
		full := msg('Q', "SELECT 1")
		starts := 0
		fr := newFrameReader(&byteAtATime{src: full})
		fr.setOnStart(func() { starts++ })
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("a byte-at-a-time Query: %v", err)
		}
		if starts != 1 {
			t.Fatalf("message starts = %d, want 1 — the length field arriving in "+
				"fragments must not re-trigger a start", starts)
		}
		if fr.midMessage() {
			t.Fatal("a message delivered one byte at a time never completed")
		}
	})

	t.Run("an undefined type byte is reported as its own fault", func(t *testing.T) {
		fr := newFrameReader(bytes.NewReader(msg('W', "")))
		_, err := io.ReadAll(fr)
		if !errors.Is(err, errUnknownFrameType) {
			t.Fatalf("err = %v, want errUnknownFrameType — a transport error here would "+
				"close the connection with nothing to say", err)
		}
		if fr.badByte() != 'W' {
			t.Fatalf("badByte = %q, want 'W'; the audit line needs the byte", fr.badByte())
		}
	})

	t.Run("a length under four is impossible, not merely short", func(t *testing.T) {
		// The length counts itself, so 3 describes no message. Left unchecked it
		// makes the body counter negative and the machine never resynchronizes.
		fr := newFrameReader(bytes.NewReader([]byte{'Q', 0, 0, 0, 3}))
		if _, err := io.ReadAll(fr); !errors.Is(err, errBadFrameLength) {
			t.Fatalf("err = %v, want errBadFrameLength", err)
		}
	})

	t.Run("mid-message is true only while a message is actually in progress", func(t *testing.T) {
		fr := newFrameReader(bytes.NewReader([]byte{'Q', 0, 0}))
		if _, err := io.ReadAll(fr); err != nil {
			t.Fatalf("a truncated header: %v", err)
		}
		if !fr.midMessage() {
			t.Fatal("a peer that sent a type byte and part of a length IS mid-frame; " +
				"reporting it idle tells an operator the client went quiet when it went slow")
		}
	})
}

// byteAtATime delivers one byte per Read, so a cell can prove the machine
// survives fragmentation the network is free to impose at any offset.
type byteAtATime struct {
	src []byte
	at  int
}

func (r *byteAtATime) Read(p []byte) (int, error) {
	if r.at >= len(r.src) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.src[r.at]
	r.at++
	return 1, nil
}
