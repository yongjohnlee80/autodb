package frontdoor

import (
	"encoding/binary"
	"errors"
	"io"
)

// THE TYPE BYTE MUST BE CHECKED ON THE PATH THE BYTES TAKE, NOT BESIDE IT.
//
// The front door has to see a frontend message's type byte BEFORE pgproto3
// decodes it: an undefined byte cannot be told from a transport failure once
// Receive has turned it into an unstructured error, and the two demand
// different answers — one closes with an accurate 08P01, the other closes with
// nothing to say (matrix row 4:Unknown-message-type-byte).
//
// The first implementation did that by handing the Backend a *bufio.Reader and
// peeking one byte from that reader beside it. That is correct for exactly one
// client: one that sends a message and waits for its reply before sending the
// next. psql behaves that way, so F1 looked right and every cell passed.
//
// It is WRONG FOR ANY CLIENT THAT PIPELINES, which is most of them — lib/pq,
// JDBC batches, and every extended-protocol client by construction.
// pgproto3.NewBackend wraps whatever reader it is given in its own chunkReader
// (backend.go: `cr := newChunkReader(r, 0)`), and chunkReader.Next fills with
//
//	io.ReadAtLeast(r.r, (*r.buf)[r.wp:], minReadCount)
//
// — it reads into the WHOLE remaining buffer and requires only the minimum. So
// while satisfying message 1 it also takes message 2, and the reader beside it
// is left empty. The peek then blocks on bytes that have already been consumed:
// the second statement is accepted by the client, never seen by the engine, and
// nobody is told, so the session hangs until the idle deadline rather than
// closing. Found by white-vision while wiring F2; it was live on main.
//
// frameReader fixes the class rather than the symptom. It sits BETWEEN the
// socket and the Backend and tracks message boundaries itself — one type byte,
// four length bytes, then length-4 body — so it validates each type byte at the
// moment it is read. The check stays before decode, it cannot be outrun by
// read-ahead because it is on the only path the bytes take, and it removes the
// requirement that the loop and the Backend share a reader, which is the
// fragility that produced the defect.
//
// It deliberately does NOT match pgproto3's "unknown message type: %c" error
// text. That would start silently passing every undefined byte through as a
// transport error the day pgx rewords it.
type frameReader struct {
	src io.Reader

	phase   framePhase
	lenBuf  [4]byte
	lenHave int
	need    int

	// onStart fires as a message's type byte is consumed — the moment the peer
	// goes from "between messages" to "mid-frame", which is what selects the
	// budget that governs it (§2). It is reported from HERE rather than inferred
	// by the loop because this is the only place that fact is observable.
	onStart func()

	// bad keeps the offending byte for the audit line. The wire is told only
	// the rule id (§1.2: a synthesized error never impersonates the target).
	bad byte
}

type framePhase int

const (
	phaseType framePhase = iota
	phaseLength
	phaseBody
)

// errUnknownFrameType is returned through Read when a type byte is not one
// PostgreSQL defines. It reaches the loop from Receive, where errors.Is tells
// it apart from a transport failure — the distinction the peek existed for.
var errUnknownFrameType = errors.New("frontdoor: undefined frontend message type")

// errBadFrameLength is a length that cannot describe a message: the length
// field counts itself, so anything under 4 is not a short frame, it is a frame
// the front door cannot parse at all.
var errBadFrameLength = errors.New("frontdoor: impossible frontend message length")

// newFrameReader wraps src for a stream of TYPED frontend messages. It is not
// for the startup phase: SSLRequest and StartupMessage carry no type byte, and
// they are read by their own Backend over the raw connection before this one
// exists.
func newFrameReader(src io.Reader) *frameReader {
	return &frameReader{src: src}
}

// setOnStart installs the message-start callback, replacing any previous one.
// Auth installs none — its own deadline governs that exchange.
func (r *frameReader) setOnStart(fn func()) { r.onStart = fn }

// badByte reports the undefined type byte that stopped the stream.
func (r *frameReader) badByte() byte { return r.bad }

func (r *frameReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		if serr := r.scan(p[:n]); serr != nil {
			// The connection ends either way, so the bytes already read do not
			// need to be handed on; reporting the framing fault immediately is
			// what keeps it distinguishable from the transport.
			return 0, serr
		}
	}
	return n, err
}

// scan walks bytes as they pass, advancing the framing state machine.
//
// It must consider every byte exactly once and must not treat body content as
// framing — a type byte inside a SQL string is just a character, which is the
// whole reason the boundaries are tracked rather than the stream searched.
func (r *frameReader) scan(b []byte) error {
	for i := 0; i < len(b); {
		switch r.phase {
		case phaseType:
			if !validFrontendType(b[i]) {
				r.bad = b[i]
				return errUnknownFrameType
			}
			if r.onStart != nil {
				r.onStart()
			}
			r.phase = phaseLength
			r.lenHave = 0
			i++

		case phaseLength:
			take := len(b) - i
			if room := 4 - r.lenHave; take > room {
				take = room
			}
			copy(r.lenBuf[r.lenHave:], b[i:i+take])
			r.lenHave += take
			i += take
			if r.lenHave == 4 {
				total := int(binary.BigEndian.Uint32(r.lenBuf[:]))
				if total < 4 {
					return errBadFrameLength
				}
				// The length counts itself. A length of exactly 4 is a complete
				// message with an empty body — Sync, Flush and Terminate are all
				// four bytes and would be mis-framed by an off-by-one here.
				r.need = total - 4
				r.lenHave = 0 // the message is framed; nothing of a length is pending
				r.phase = phaseBody
				if r.need == 0 {
					r.phase = phaseType
				}
			}

		case phaseBody:
			take := len(b) - i
			if take > r.need {
				take = r.need
			}
			r.need -= take
			i += take
			if r.need == 0 {
				r.phase = phaseType
			}
		}
	}
	return nil
}

// midMessage reports whether the peer is part-way through a message.
//
// lenHave is cleared as soon as a length is complete, not only when the next
// type byte arrives: leaving it set made every COMPLETED message look like one
// still in progress, so the first idle session after any message was reported
// as a frame stall — an operator sent to chase a slow client that was simply
// not asking for anything. Caught by the idle-expiry cell, which is what that
// cell is for.
//
// This is what selects the budget an expiry is charged to, and it is a fact
// about the STREAM rather than an inference from where the loop happened to be
// — which is what the old peek-succeeded/peek-failed split was.
func (r *frameReader) midMessage() bool {
	return r.phase != phaseType || r.lenHave != 0
}
