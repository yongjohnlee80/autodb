package frontdoor

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// The startup exchange (protocol matrix §2 rows 2.1–2.5a).
//
// This is everything before authentication: TLS negotiation, the refusals
// that happen without a credential ever being read, and the StartupMessage
// itself. Authentication is the next slice; this one ends by denying, which
// is the honest state of a listener that has no credential store yet.

// Pre-auth bounds (ADR-0075 §4 defaults table). Deliberately tighter than
// the post-auth caps: nothing here has authenticated, so the budget an
// anonymous peer can command should be the smallest one in the system.
const (
	// PreAuthMaxBodyLen bounds a single pre-auth message. Distinct from the
	// post-auth SetMaxBodyLen precisely so the pre-auth arithmetic is honest
	// — 64 connections at 64 MiB would be a very different number.
	PreAuthMaxBodyLen = 64 * 1024

	// TLSHandshakeDeadline, StartupDeadline bound the phases before auth.
	// Each is its own deadline rather than one budget for the lot, so a peer
	// cannot spend the whole allowance on the handshake and still be owed
	// time for the startup packet.
	TLSHandshakeDeadline = 10 * time.Second
	StartupDeadline      = 10 * time.Second
)

// deadlines is the phase budget one connection runs under.
//
// A struct rather than the constants read directly, so a cell can shorten
// them and still exercise the REAL enforcement path. The alternative is a
// slowloris cell that waits ten seconds per case, which is the kind of cost
// that gets a suite marked short and then skipped — a deadline nobody tests
// because testing it is slow is a deadline nobody has.
type deadlines struct {
	tls     time.Duration
	startup time.Duration
	auth    time.Duration
	idle    time.Duration
}

func defaultDeadlines() deadlines {
	return deadlines{
		tls:     TLSHandshakeDeadline,
		startup: StartupDeadline,
		auth:    AuthDeadline,
		idle:    IdleDeadline,
	}
}

// Protocol version constants. autodb speaks 3.0 and negotiates 3.x down to
// it, per matrix row 2.5.
const (
	protocolMajor3    = 3
	protocolVersion30 = uint32(protocolMajor3)<<16 | 0
)

// startupOutcome is what the exchange produced. Exactly one of Params or
// Denied is meaningful.
type startupOutcome struct {
	// Params is the accepted StartupMessage parameter set.
	Params map[string]string
	// Denied is the internal reason, empty when the exchange succeeded.
	Denied denialReason
	// Negotiated records that a NegotiateProtocolVersion was sent — the
	// client asked for a 3.x we do not implement and continues at 3.0.
	Negotiated bool
	// RefusedParam names the startup parameter that failed §3.1, for the
	// audit row only. The wire gets the uniform denial: telling a caller
	// WHICH parameter this server dislikes would map the accepted set for
	// anyone willing to ask repeatedly.
	RefusedParam string
}

// errCancelRequest reports a CancelRequest, which is not a session at all:
// it is a plaintext control connection that is answered and closed.
var errCancelRequest = errors.New("frontdoor: cancel request")

// runStartup performs the exchange on a freshly accepted connection and
// returns either the accepted parameters or the reason it was denied.
//
// It never reads an authentication response: TLS is established first,
// without exception (ADR-0075 §4). The ordering is not a preference — a
// credential read before TLS is a credential an active MITM already has, and
// this surface's whole credential model assumes the wire is private.
func runStartup(raw net.Conn, tlsCfg *tls.Config, now func() time.Time, dl deadlines) (*tls.Conn, startupOutcome, error) {
	// The pre-auth backend reads the SSLRequest from the PLAINTEXT
	// connection. It is bounded before it reads anything, so a peer's first
	// act cannot be to name a length we would then allocate for.
	// A buffered reader so the first bytes can be INSPECTED before being
	// consumed. Direct TLS has to be identified by what it actually is,
	// which is the fix for having previously identified it by a symptom it
	// shares with ordinary oversize frames.
	br := bufio.NewReader(raw)
	plain := pgproto3.NewBackend(br, raw)
	plain.SetMaxBodyLen(PreAuthMaxBodyLen)

	// S0 is a LOOP, not a single read.
	//
	// It has to be: matrix row 2.2 refuses GSS encryption with 'N' and then
	// lets the client carry on — 'N' is the protocol's own way of declining
	// an option, and libpq's next move after it is to ask for TLS. Answering
	// 'N' and closing turned a declined option into a dead connection, so a
	// client that asked for GSS first could never reach TLS at all. My own
	// test prose claimed 'N' let the client proceed while the code hung up
	// on it.
	//
	// Bounded by the same deadline and the same pre-auth cap on every pass,
	// so the loop cannot become a way to hold a socket open for free.
	var sawGSS bool
	for {
		if err := raw.SetDeadline(now().Add(dl.tls)); err != nil {
			return nil, startupOutcome{}, err
		}
		// Direct TLS is recognised by its ClientHello SIGNATURE, before any
		// parse is attempted (matrix row 2.1a).
		//
		// The first version inferred it from pgproto3's "invalid length"
		// error, which a TLS ClientHello does produce — and so does an
		// ordinary length-prefixed frame that exceeds the pre-auth cap. So a
		// client that simply sent something too big was audited as a
		// PostgreSQL 17 direct-TLS attempt, sending an operator to look for
		// a client that may not exist on their network. Meanwhile
		// reasonPreAuthOversize sat unused in the source, which is the same
		// tell as the unapplied parameter policy last round: a named reason
		// nobody emits is a case nobody handles.
		// The frame is classified from its OWN BYTES before pgproto3 reads
		// it — the declared length and, where present, the request code.
		//
		// This replaces matching on the library's error text, which was
		// wrong twice for the same reason. "invalid length" is emitted for
		// an over-cap frame AND for an underlength one, so a length of 0 or
		// 3 was audited as pre-auth-message-too-large: the opposite of what
		// it is. Before that, the same text was read as direct TLS. Both
		// times I was inferring a cause from a symptom shared by several
		// causes, when the distinguishing value was sitting in the bytes.
		//
		// A dependency's error strings are also not a contract: they can be
		// reworded in a patch release and take this classification with them.
		if head, perr := br.Peek(2); perr == nil && isTLSClientHello(head) {
			return nil, startupOutcome{}, tlsFailure(reasonDirectTLS.String())
		}
		if head, perr := br.Peek(4); perr == nil {
			if reason, bad := classifyStartupLength(binary.BigEndian.Uint32(head)); bad {
				return nil, startupOutcome{}, tlsFailure(reason)
			}
		}
		first, err := plain.ReceiveStartupMessage()
		if err != nil {
			// Row 2.1a: PostgreSQL 17's direct-TLS negotiation opens with a
			// TLS ClientHello rather than a length-prefixed request, so it
			// arrives here as a parse failure. It is refused in v1 to keep
			// ONE negotiation path under test.
			//
			// It is a TLS failure, not an authentication denial, and the
			// difference is not cosmetic: the peer never presented a
			// credential, so calling it fd.auth_denied puts a
			// non-authentication event in the trail an operator reads to
			// count credential attacks. It also means no denial FRAME is
			// written — a client speaking TLS cannot read a PostgreSQL
			// error, so sending one is noise on the wire and a lie in the log.
			return nil, startupOutcome{}, s0Failure(err)
		}

		switch msg := first.(type) {
		case *pgproto3.SSLRequest:
			// Row 2.1: answer 'S' and begin TLS.
			if _, werr := raw.Write([]byte{'S'}); werr != nil {
				return nil, startupOutcome{}, werr
			}
		case *pgproto3.GSSEncRequest:
			// Row 2.2: decline with 'N' and KEEP READING. A second
			// GSSENCRequest is a client going in circles; refuse that.
			if sawGSS {
				return nil, startupOutcome{Denied: reasonStartupMalformed}, nil
			}
			sawGSS = true
			if _, werr := raw.Write([]byte{'N'}); werr != nil {
				return nil, startupOutcome{}, werr
			}
			continue
		case *pgproto3.CancelRequest:
			// Row 2.3: cancel connections are plaintext by protocol.
			return nil, startupOutcome{}, errCancelRequest
		case *pgproto3.StartupMessage:
			// Row 2.1 error path: a plaintext StartupMessage. No fallback —
			// this surface has no unencrypted mode to fall back to.
			_ = msg
			return nil, startupOutcome{Denied: reasonPlaintextStartup}, nil
		default:
			return nil, startupOutcome{Denied: reasonStartupMalformed}, nil
		}
		break
	}

	// TLS, on its own deadline.
	if err := raw.SetDeadline(now().Add(dl.tls)); err != nil {
		return nil, startupOutcome{}, err
	}
	secure := tls.Server(raw, tlsCfg)
	if err := secure.Handshake(); err != nil {
		// Classified rather than wrapped, so the audit trail carries a
		// STABLE reason an operator can count and the library's wording
		// rides in the detail. A reason string that changes when a
		// dependency rewords an error is a reason nobody can alert on.
		return nil, startupOutcome{}, tlsFailureDetail("tls-handshake", err.Error())
	}

	// Everything from here is inside TLS.
	if err := secure.SetDeadline(now().Add(dl.startup)); err != nil {
		return secure, startupOutcome{}, err
	}
	be := pgproto3.NewBackend(secure, secure)
	be.SetMaxBodyLen(PreAuthMaxBodyLen)

	// The startup packet's VERSION is read here rather than delegated,
	// because pgproto3's ReceiveStartupMessage accepts only 3.0 and 3.2 and
	// returns "unknown startup message code" for everything else.
	//
	// That is a reasonable library default and the wrong policy for this
	// surface. Matrix row 2.5 requires ANY 3.x minor to be negotiated DOWN
	// and continued — "pg-conformant, never a hard refusal" — which is what
	// PostgreSQL itself does; under the library's allowlist a client asking
	// for 3.1 or 3.3 would be refused outright even though it can speak 3.0
	// perfectly well. Row 2.5a's unsupported-major refusal is likewise
	// unreachable through it: the error arrives before any version check of
	// mine can run.
	//
	// So the length and version are read directly and the PARAMETERS are
	// still decoded by the library — owning the fiddly part is not the goal,
	// owning the policy the matrix pins is.
	raw2, err := readStartupPacket(secure)
	if err != nil {
		return secure, startupOutcome{Denied: reasonStartupMalformed}, nil
	}
	version := binaryBigEndianUint32(raw2)

	switch version {
	case sslRequestCode:
		// A second SSLRequest inside TLS is a protocol violation (row 2.1).
		return secure, startupOutcome{Denied: reasonStartupMalformed}, nil
	case cancelRequestCode:
		return secure, startupOutcome{}, errCancelRequest
	}
	if version>>16 != protocolMajor3 {
		// Row 2.5a: an unsupported major is a refusal, not a negotiation.
		// There is no 3.0 for a major-4 client to fall back to.
		return secure, startupOutcome{Denied: reasonUnsupportedMajor}, nil
	}

	// Parameters decode first: row 2.5's NegotiateProtocolVersion must name
	// the unrecognized `_pq_.*` options, and they live in the parameter
	// block, so the block has to be read before the answer can be composed.
	// The version word is rewritten to 3.0 for the decode — the parameter
	// block's own format does not vary by minor.
	negotiated := version != protocolVersion30
	putUint32(raw2, protocolVersion30)
	var sm pgproto3.StartupMessage
	if derr := sm.Decode(raw2); derr != nil {
		return secure, startupOutcome{Denied: reasonStartupMalformed}, nil
	}
	out := startupOutcome{Params: sm.Parameters}

	// A `_pq_.*` parameter is a protocol EXTENSION the client is asking for.
	// autodb implements none, and saying so by name is the point of the
	// message: a client told "3.0, and I did not understand these three
	// options" can decide what to do, while silence leaves it believing an
	// extension it depends on was accepted.
	unrecognized := unrecognizedProtocolOptions(sm.Parameters)
	if negotiated || len(unrecognized) > 0 {
		// Row 2.5: negotiate DOWN and continue. A newer client asks for more
		// and accepts less by design; refusing it would break clients that
		// are perfectly able to speak 3.0.
		be.Send(&pgproto3.NegotiateProtocolVersion{
			NewestMinorProtocol: 0,
			UnrecognizedOptions: unrecognized,
		})
		if ferr := be.Flush(); ferr != nil {
			return secure, startupOutcome{}, ferr
		}
		out.Negotiated = true
	}

	// §3.1: the accepted set is CLOSED, and it is checked AFTER the protocol
	// answer is sent. NegotiateProtocolVersion answers a question about the
	// PROTOCOL — a client that asked for 3.2 is owed "3.0, and here is what I
	// did not understand" whether or not its parameters then fail policy.
	// Withholding it would leave a client guessing about the protocol
	// because of an unrelated parameter.
	//
	// The check is still BEFORE the
	// connection can fall through to any later denial. The first version
	// decoded the parameters and returned them unvalidated, so a startup
	// carrying `search_path` was denied for want of a credential store
	// rather than for the parameter, and reasonStartupParamRefus sat unused
	// in the source — a dead constant is a policy nobody is applying.
	if refused, ok := checkStartupParams(sm.Parameters); !ok {
		_ = refused // named in the audit by the caller; never on the wire
		return secure, startupOutcome{Denied: reasonStartupParamRefus, RefusedParam: refused, Negotiated: out.Negotiated}, nil
	}
	return secure, out, nil
}

// unrecognizedProtocolOptions returns the `_pq_.*` parameter names, sorted so
// the answer is deterministic — a message whose contents vary by map
// iteration order is one nobody can write a test against.
func unrecognizedProtocolOptions(params map[string]string) []string {
	var out []string
	for k := range params {
		if strings.HasPrefix(k, "_pq_.") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Startup packet codes pgproto3 does not export.
const (
	sslRequestCode    = 80877103
	cancelRequestCode = 80877102
	gssEncRequestCode = 80877104
)

// readStartupPacket reads one length-prefixed startup packet body, bounded
// by the pre-auth cap.
//
// The bound is applied to the LENGTH FIELD before anything is allocated:
// a peer's first act on this surface must not be to name a size we then
// reserve for them. That is the whole reason the pre-auth cap is a separate,
// much smaller number than the post-auth one.
func readStartupPacket(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint32(lenBuf[:])) - 4
	if size < 4 || size > PreAuthMaxBodyLen {
		return nil, fmt.Errorf("frontdoor: startup packet length %d out of bounds", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func binaryBigEndianUint32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func putUint32(b []byte, v uint32)          { binary.BigEndian.PutUint32(b, v) }

// sendDenial writes the uniform denial and flushes it.
//
// Separated from the decision so every refusal path emits the SAME bytes:
// one construction site is what makes "indistinguishable across causes" a
// property of the code rather than a habit of whoever wrote each branch.
func sendDenial(w interface {
	Write([]byte) (int, error)
}, reason denialReason) error {
	be := pgproto3.NewBackend(emptyReader{}, w)
	be.Send(denial(reason))
	return be.Flush()
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, errors.New("frontdoor: no reader") }

// tlsFailure marks an S0 failure that is NOT an authentication event.
//
// The peer never presented a credential, so recording it as fd.auth_denied
// would put a non-authentication event in the trail an operator reads to
// count credential attacks — and no denial frame is written either, because a
// client speaking raw TLS cannot read a PostgreSQL error message.
type tlsFailErr struct {
	reason string
	detail string
	// attributable reports whether the PEER caused this.
	//
	// A connection that opened and went away before sending a byte did not
	// fail anything — that is what a TCP health check looks like from here,
	// and charging it to the source address would throttle an operator's own
	// monitoring out of the estate within a minute. A peer that sent bytes we
	// refused, or that held the socket open until the deadline, is the one
	// row 2.1b means.
	attributable bool
}

func (e tlsFailErr) Error() string { return "frontdoor: " + e.reason }

func tlsFailure(reason string) error {
	return tlsFailErr{reason: reason, attributable: true}
}

func tlsFailureDetail(reason, detail string) error {
	return tlsFailErr{reason: reason, detail: detail, attributable: true}
}

// peerGone marks a connection that ended before it asked for anything.
func peerGone(reason string) error {
	return tlsFailErr{reason: reason, attributable: false}
}

// classifyStartupLength decides an S0 frame's fate from its DECLARED length.
//
// Underlength and over-cap are different failures and must not share a
// reason: an under-length frame is malformed, and calling it "too large" is
// not merely imprecise, it points an operator in the opposite direction.
//
// The declared value is a length INCLUDING its own four bytes, so the body is
// four less. Below the four bytes needed for a version or request code there
// is nothing a startup packet could be.
func classifyStartupLength(declared uint32) (reason string, bad bool) {
	body := int64(declared) - 4
	switch {
	case body < startupMinBody:
		return reasonStartupMalformed.String(), true
	case body > PreAuthMaxBodyLen:
		return reasonPreAuthOversize.String(), true
	default:
		return "", false
	}
}

// startupMinBody is the smallest possible startup body: one 32-bit version or
// request code.
const startupMinBody = 4

// s0Failure names an S0 read failure that length classification did not
// already catch, and says whether the peer is answerable for it.
//
// An EOF before any byte arrived is a connection that opened and closed —
// a port scan, a load balancer's health probe, a client that changed its
// mind. It is audited, because an operator may want to see it, and it is NOT
// charged to the source address, because throttling a health check for being
// a health check is an outage we would have configured ourselves.
func s0Failure(err error) error {
	switch {
	case strings.Contains(err.Error(), "unknown startup message code"):
		return tlsFailure("startup-code-unknown")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return peerGone("peer-gone-before-startup")
	default:
		return tlsFailure("startup-unreadable")
	}
}

// isTLSClientHello reports whether these bytes open a TLS record.
//
// 0x16 is the handshake content type and 0x03 the legacy record-version
// major that every TLS version still puts on the wire, TLS 1.3 included. Two
// bytes is enough to tell a TLS record from a PostgreSQL startup packet,
// whose first four bytes are a length: a startup packet beginning 0x16 0x03
// would claim a length of roughly 369 million, which the pre-auth cap
// refuses anyway.
func isTLSClientHello(head []byte) bool {
	return len(head) >= 2 && head[0] == 0x16 && head[1] == 0x03
}
