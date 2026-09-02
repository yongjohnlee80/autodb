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

	// buf holds bytes read to FRAME A HEADER that the Backend has not taken yet.
	// waitHeader reads ahead so the loop can admit a frame header-first; those
	// bytes are still owed to pgproto3, so Read serves them before touching src.
	// Without this, reading a header would consume bytes the Backend needs and
	// the stream would split in two — the failure the shared-reader peek caused.
	buf []byte
	// deferred is a framing fault waitHeader found while reading ahead. The
	// bytes it scanned are already buffered, and Read serves those WITHOUT
	// re-scanning (they have been scanned once), so the fault has to be carried
	// or it is lost — which is what an undefined type byte did on the first
	// version of this: it reached pgproto3 unvalidated and the accurate 08P01
	// became a bare connection reset.
	deferred error

	// skip counts bytes of a REFUSED frame still to be discarded rather than
	// delivered. They are still SCANNED — the framing state machine must stay
	// aligned or the next header lands at the wrong offset — but they never
	// reach pgproto3, which is what stops a refused 64 MiB body being decoded.
	skip int

	// delivered counts bytes handed to the Backend, so a cell can MEASURE that a
	// refused frame's body was never delivered. The property is a resource one —
	// from the wire a refusal looks the same whether or not the body was decoded
	// — so it has to be measured rather than asserted (lector r0).
	delivered int

	// skipped records that the last admission decision was a refusal.
	skipped bool

	// bounded turns the delivery boundary ON, and it is ON ONLY INSIDE
	// runSession. auth and defaultSession use this same reader and admit
	// nothing, so a boundary applying to them would starve the credential
	// exchange and the no-Queries path (lector's Receive-site audit: exactly
	// three sites, and runExtended is not one — it is reached only after
	// runSession's receive).
	bounded bool

	// deliverable bounds what may reach pgproto3 while bounded: the bytes of the
	// frame the loop has ADMITTED, and not one more.
	//
	// Handing over whole buffers let pgproto3's chunkReader read ahead and
	// privately buffer future BODIES; at a crossing header there was then
	// nothing left for the skip to discard, so it consumed the following Sync
	// and the segment desynchronised. A frame the loop has not admitted must not
	// be inside pgproto3 at all.
	deliverable int

	phase   framePhase
	lenBuf  [4]byte
	lenHave int
	need    int

	// onStart fires as a message's type byte is consumed — the moment the peer
	// goes from "between messages" to "mid-frame", which is what selects the
	// budget that governs it (§2). It is reported from HERE rather than inferred
	// by the loop because this is the only place that fact is observable.
	onStart func()

	// pending queues the header of every frame this reader has FRAMED but that no
	// Receive has consumed yet.
	//
	// IT IS A QUEUE AND NOT A CALLBACK, because scan runs far ahead of the loop
	// (lector C r2 MF2). One socket read can carry [Describe, Sync, Describe],
	// and scan frames all three before Receive returns the first. A callback
	// charging a live segment as each was framed therefore charged the THIRD
	// frame's bytes into the FIRST frame's segment — falsely refusing a compliant
	// frame — and the Sync between them then reset the counters and erased the
	// charge the third frame had already been given, so it was admitted for
	// nothing. Both directions of wrong, out of a single read.
	//
	// Queuing preserves the framing order and lets the loop apply each header
	// when it reaches that frame, so a Sync's reset lands between the frames it
	// actually separates.
	pending []frameHeader

	// typ is the current message's type byte, held from phaseType so the queued
	// header can carry it alongside the length.
	typ byte

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

// frameHeader is what a frame declared itself to be, before any of its body was
// decoded: its type byte and the length it announced.
type frameHeader struct {
	typ      byte
	declared int
}

// peekHeader returns the header of the next frame a Receive will return, when
// this reader has already framed it. Not framed yet means those bytes have not
// arrived, and the caller must Receive to make them arrive.
func (r *frameReader) peekHeader() (frameHeader, bool) {
	if len(r.pending) == 0 {
		return frameHeader{}, false
	}
	return r.pending[0], true
}

// consumeHeader removes the head of the queue.
//
// EVERY Receive must call it, auth's included (lector C r2 MF3). Auth shares
// this reader and its Backend, so a queue that only the session loop advances
// attributes every later header to the wrong frame — and read-ahead during auth
// means a post-auth frame can already be queued before the loop exists, which is
// exactly the frame that then went uncharged.
func (r *frameReader) consumeHeader() (frameHeader, bool) {
	if len(r.pending) == 0 {
		return frameHeader{}, false
	}
	h := r.pending[0]
	r.pending = r.pending[1:]
	return h, true
}

// badByte reports the undefined type byte that stopped the stream.
func (r *frameReader) badByte() byte { return r.bad }

func (r *frameReader) Read(p []byte) (int, error) {
	// A refused frame is discarded before anything else is served, so its body
	// never reaches the Backend.
	if r.skip > 0 {
		if err := r.drainSkip(); err != nil {
			return 0, err
		}
	}
	room := len(p)
	if r.bounded && r.deliverable < room {
		room = r.deliverable
	}
	if room <= 0 {
		// Nothing admitted. This is reached when the loop could not frame a
		// header — a closed connection — so the read goes THROUGH to surface the
		// transport's own error. Blocking here replaced a clean EOF with a
		// front-door error and turned ordinary disconnects into failures, which
		// is what broke the extended cells on the first attempt.
		room = len(p)
	}
	// Buffered bytes first: they were read to frame a header and are still owed.
	if len(r.buf) > 0 {
		n := copy(p[:room], r.buf)
		r.buf = r.buf[n:]
		r.delivered += n
		if r.deliverable >= n {
			r.deliverable -= n
		}
		return n, nil
	}
	// The buffer is drained; a fault found while reading ahead surfaces now,
	// with its own identity rather than as a transport failure.
	if r.deferred != nil {
		err := r.deferred
		r.deferred = nil
		return 0, err
	}
	n, err := r.src.Read(p[:room])
	if n > 0 {
		r.delivered += n
		if r.deliverable >= n {
			r.deliverable -= n
		}
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
			r.typ = b[i]
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
				// QUEUED HERE, before a byte of the body is passed on, so the
				// header reaches the loop no later than the frame it describes.
				r.pending = append(r.pending, frameHeader{typ: r.typ, declared: total})
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

// waitHeader reads until the NEXT frame's header is framed, so a caller can
// decide about a frame before its body is delivered.
//
// WHY IT EXISTS. Segment admission is header-first when the header happens to be
// framed already, and fell back to deciding AFTER Receive when it was not — so
// the crossing frame's body had been decoded before it was refused. That was
// bounded while the post-auth cap was (wrongly) the 64 KiB pre-auth one; at the
// documented 64 MiB the same residual becomes a 64 MiB decode, which is why the
// cap raise and this must land together.
//
// The bytes it reads are BUFFERED, not consumed: they are still owed to
// pgproto3, and a reader that swallowed them would split the stream in two —
// the exact failure the old peek-a-shared-reader design caused.
//
// It returns false when the connection ends before a header completes; the
// caller's Receive then reports the same condition with its own identity.
func (r *frameReader) waitHeader() bool {
	if len(r.pending) > 0 {
		return true
	}
	// AT MOST THE HEADER: one byte at a time is the only size that cannot
	// overshoot a five-byte header and put an unadmitted body out of reach.
	tmp := make([]byte, 1)
	for len(r.pending) == 0 {
		n, err := r.src.Read(tmp)
		if n > 0 {
			chunk := append([]byte(nil), tmp[:n]...)
			if serr := r.scan(chunk); serr != nil {
				// DO NOT buffer these. The connection ends either way, and
				// handing them on lets pgproto3 decode the bad frame itself —
				// its "unknown message type" then MASKS the sentinel, and the
				// loop reports a transport failure instead of an accurate
				// 08P01. That is the same masking the reader exists to prevent.
				r.deferred = serr
				return false
			}
			r.buf = append(r.buf, chunk...)
		}
		if err != nil {
			return false
		}
	}
	return true
}

// skipFrame tells the reader to discard a refused frame ENTIRELY — its header
// and its declared body — instead of handing it to pgproto3.
//
// This is the body-skip. Deciding header-first was necessary and not sufficient:
// admission refused the frame and set the segment discarding, but the loop still
// called Receive, so the crossing body was decoded before the discard branch
// applied (lector r0). Moving the decision earlier changed WHEN we refuse, not
// WHETHER we decode — and a surviving mutation told me so, which I first read as
// a missing cell rather than a missing feature.
func (r *frameReader) skipFrame(h frameHeader) {
	r.skipped = true
	// One type byte plus the declared length, which counts its own four bytes.
	r.skip += 1 + h.declared
	// POP THE REFUSED HEADER. Every Receive pops exactly one header (lector C r2
	// MF3) — but the refused frame never reaches Receive, so the next Receive
	// returns the frame AFTER it. Leaving this header queued makes the loop's
	// consumeHeader attribute the refused frame's header to the Sync that
	// followed, and the queue is off by one for the rest of the segment: the
	// Sync's readiness never arrives.
	if len(r.pending) > 0 {
		r.pending = r.pending[1:]
	}
}

// drainSkip consumes and discards the pending skip, SCANNING as it goes.
//
// Scanning matters: the framing state machine tracks where each body ends, so
// bytes discarded without scanning would leave `need` un-decremented and the
// next header would be read at the wrong offset — turning a refusal into a
// desynchronised stream, which is worse than the decode it avoids.
func (r *frameReader) drainSkip() error {
	scratch := make([]byte, 4096)
	for r.skip > 0 {
		if len(r.buf) > 0 {
			n := r.skip
			if n > len(r.buf) {
				n = len(r.buf)
			}
			r.buf = r.buf[n:]
			r.skip -= n
			continue
		}
		n, err := r.src.Read(scratch)
		if n > 0 {
			chunk := append([]byte(nil), scratch[:n]...)
			if serr := r.scan(chunk); serr != nil {
				r.skip = 0
				return serr
			}
			take := r.skip
			if take > len(chunk) {
				take = len(chunk)
			}
			r.skip -= take
			// Whatever followed the refused frame is the next frame's, and is
			// owed to the Backend.
			if rest := chunk[take:]; len(rest) > 0 {
				r.buf = append(r.buf, rest...)
			}
		}
		if err != nil {
			r.skip = 0
			return err
		}
	}
	return nil
}

// deliveredBytes reports how much has reached the Backend, for cells measuring
// that a refused frame's body did not.
func (r *frameReader) deliveredBytes() int { return r.delivered }

// allow admits a frame for delivery: pgproto3 may now see exactly its bytes.
func (r *frameReader) allow(h frameHeader) { r.deliverable += 1 + h.declared }

// setBounded turns the delivery boundary on. runSession only.
func (r *frameReader) setBounded(on bool) { r.bounded = on }

// wasSkipped reports whether the last admission decision was a refusal, so the
// loop does not allow a frame it just skipped.
func (r *frameReader) wasSkipped() bool {
	was := r.skipped
	r.skipped = false
	return was
}
