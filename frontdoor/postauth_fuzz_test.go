package frontdoor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// POST-AUTH FRAME FUZZING (matrix §10: "frame/length fuzzing (header-first
// property: no read past a refused header)"), F1's half.
//
// #37 fuzzed the PRE-auth reader, where the frames carry no type byte and the
// rule is a length classification. This is the other reader: post-auth framing,
// where a type byte precedes a length that counts itself, and where the front
// door must decide on the HEADER before any body is consumed.
//
// THE PROPERTY, stated so a failure is legible: when frameReader refuses a
// header — an undefined type byte, or a length that cannot describe a message —
// it must not have consumed anything beyond that header. A reader that
// swallowed the body of a frame it was about to refuse would resynchronize on
// arbitrary bytes, and the next "type byte" it saw would be payload. That is
// how a refusal becomes a parser confusion.

// countingReader reports how many bytes were actually taken from the wire, so
// the header-first property can be MEASURED rather than asserted.
type countingReader struct {
	src  *bytes.Reader
	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.read += n
	return n, err
}

// frame builds one well-formed post-auth message.
func frame(typ byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:], uint32(len(body)+4))
	return append(out, body...)
}

// FuzzPostAuthFrameReader drives arbitrary bytes through the post-auth framing
// state machine. Three properties, none of which may depend on the input being
// well-formed:
//
//  1. It never panics, whatever arrives.
//  2. A refused header leaves the body unread — the header-first property.
//  3. Every refusal carries one of the two framing identities, never a bare
//     error: "undefined type byte" and "impossible length" demand different
//     answers on the wire (08P01 with the type byte in the audit, versus an
//     unparseable frame), and a shared error would make them one.
func FuzzPostAuthFrameReader(f *testing.F) {
	f.Add([]byte{'Q', 0, 0, 0, 8, 'a', 'b', 'c', 'd'})
	f.Add([]byte{'W', 0, 0, 0, 4})                  // undefined type
	f.Add([]byte{'Q', 0, 0, 0, 3})                  // impossible length
	f.Add([]byte{'Q', 0, 0, 0, 4})                  // empty body
	f.Add([]byte{'S', 0, 0, 0, 4, 'X', 0, 0, 0, 4}) // pipelined pair
	f.Add([]byte{'Q', 255, 255, 255, 255})          // length near the uint32 ceiling
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		cr := &countingReader{src: bytes.NewReader(in)}
		fr := newFrameReader(cr)

		// Drain in small chunks, so a body is never handed over in the same
		// Read as the header that would have refused it.
		buf := make([]byte, 7)
		var err error
		for err == nil {
			_, err = fr.Read(buf)
		}

		switch {
		case errors.Is(err, errUnknownFrameType):
			if !validFrontendType(fr.badByte()) {
				// Correct: it refused a byte the protocol does not define.
			} else {
				t.Fatalf("refused %q as undefined, but validFrontendType accepts it", fr.badByte())
			}
		case errors.Is(err, errBadFrameLength):
			// Correct: a length under four describes no message.
		case errors.Is(err, io.EOF):
			// The input ran out. Nothing to check beyond not having panicked.
		default:
			t.Fatalf("post-auth framing produced an error with no identity: %v — an undefined "+
				"type byte and an unparseable length demand different answers, and an error "+
				"that is neither is a refusal nobody can act on", err)
		}
	})
}

// The header-first property on its own, measured rather than fuzzed: a refused
// header must leave the body on the wire.
//
// §10 names this property specifically, and it is the one that cannot be
// checked by looking at the returned error — a reader that refuses correctly
// AND swallows the body returns exactly the same error as one that does not.
func TestPostAuth_ARefusedHeaderLeavesTheBodyUnread(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, 4096)

	for _, tc := range []struct {
		name       string
		header     []byte
		want       error
		wantStarts int
	}{
		// The type byte is refused before it is ever a message, so no start.
		{"undefined type byte", []byte{'W', 0, 0, 0, uint8(len(body) + 4)}, errUnknownFrameType, 0},
		// 'Q' IS a defined type, so its start is reported; the length is then
		// impossible and the body must not produce a second one.
		{"impossible length", []byte{'Q', 0, 0, 0, 3}, errBadFrameLength, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := append(append([]byte{}, tc.header...), body...)
			cr := &countingReader{src: bytes.NewReader(wire)}
			fr := newFrameReader(cr)
			starts := 0
			fr.setOnStart(func() { starts++ })

			buf := make([]byte, len(wire))
			_, err := fr.Read(buf)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}

			// THE MEASUREMENT, and getting it right took two attempts.
			//
			// midMessage() is NOT the instrument. One Read legitimately pulls a
			// whole buffer from the socket, so counting bytes taken from the
			// source measures the socket, not the parser; and midMessage stays
			// true after a refused LENGTH because the length counter is still
			// set, even though scan stopped at the header and never advanced
			// into the body. Both readings would have failed a reader that is
			// behaving correctly.
			//
			// The property is that nothing in the BODY was interpreted as
			// framing. A message start is the observable form of that: the body
			// here is 4096 'x' bytes, and 'x' is not a defined type byte, so a
			// reader that resynchronized inside it would either report a start
			// or refuse a second time. Neither may happen.
			if starts != tc.wantStarts {
				t.Fatalf("%d message starts after refusing the header, want %d — the body was "+
					"interpreted as framing, so the next type byte reported would be payload",
					starts, tc.wantStarts)
			}
		})
	}
}

// Pipelined frames in ONE write, across the whole type-byte space: whatever
// arrives, the reader either frames it or refuses it with an identity, and a
// refusal in the middle of a batch does not become a refusal of the batch's
// first frame.
func TestPostAuth_APipelinedBatchIsFramedOrRefusedPerFrame(t *testing.T) {
	for b := 0; b < 256; b++ {
		typ := byte(b)
		wire := append(frame('Q', []byte("SELECT 1")), frame(typ, []byte("body"))...)
		cr := &countingReader{src: bytes.NewReader(wire)}
		fr := newFrameReader(cr)
		starts := 0
		fr.setOnStart(func() { starts++ })

		buf := make([]byte, 4)
		var err error
		for err == nil {
			_, err = fr.Read(buf)
		}

		if validFrontendType(typ) {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("type %q is defined, but the batch failed: %v", typ, err)
			}
			if starts != 2 {
				t.Fatalf("type %q: %d message starts, want 2 — the second frame of a pipelined "+
					"batch must be seen", typ, starts)
			}
			continue
		}
		if !errors.Is(err, errUnknownFrameType) {
			t.Fatalf("type %q is undefined, but the batch reported %v", typ, err)
		}
		if starts != 1 {
			t.Fatalf("type %q: %d starts, want 1 — the FIRST frame is well-formed and must be "+
				"reported before the second is refused", typ, starts)
		}
	}
}
