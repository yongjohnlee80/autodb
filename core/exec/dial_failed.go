package exec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"

	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// A REQUEST THAT CANNOT REACH ITS BACKEND MUST FAIL THE REQUEST AND LEAVE THE
// SESSION USABLE, AND IT MUST SAY SO IN ONE FIXED SHAPE THAT NAMES NO HOST,
// DATABASE, ROLE OR CATALOG DETAIL.
//
// What goes wrong otherwise is two separate leaks through one hole. A request
// whose backend acquisition fails used to return the driver's own error, and
// the front door's refusal renderer has a default arm that puts the error's
// text on the wire: the client learned the target hostname, the port, the
// database name and — when the failure was upstream authentication — the role
// autodb connects as. None of that is the client's to know; a caller with a
// TCP route and a broken target could map the install without ever holding a
// credential for it. The second leak is the SQLSTATE: the default arm answers
// 42501, which tells a client its PRIVILEGES are wrong when in fact nothing
// about the statement was ever judged.
//
// THE OBVIOUS ALTERNATIVE — forwarding the target's own error, the way a real
// PostgreSQL proxy forwards a target error for a statement that ran — IS
// WRONG HERE, and the difference is who produced the error. A target error for
// a statement is the target answering the client's own work, and the client is
// entitled to it. A dial failure happened before any of the client's work
// reached anything; the text describes OUR topology, not their statement.
//
// So every dial failure produces the identical client shape and the stage and
// the raw cause exist only in the audit, where an operator can tell a DNS
// failure from a TLS one from an upstream authentication failure and the
// client cannot.

// DialStage names WHICH part of opening a backend connection failed. It is an
// audit fact: the wire shape is the same for every value here, and that
// uniformity is the point.
type DialStage string

const (
	// DialStageResolve is a name that would not resolve.
	DialStageResolve DialStage = "resolve"
	// DialStageConnect is a transport that would not open: refused, timed out,
	// unreachable.
	DialStageConnect DialStage = "connect"
	// DialStageTLS is a transport that opened and a TLS handshake that did not
	// complete, including certificate verification.
	DialStageTLS DialStage = "tls"
	// DialStageAuthenticate is the target refusing the credential autodb
	// presented, or the database it asked for.
	DialStageAuthenticate DialStage = "authenticate"
	// DialStageStartup is the rest of the startup exchange: an unsupported
	// protocol version, a refused startup parameter, anything the target
	// raised before the connection was usable.
	DialStageStartup DialStage = "startup"
	// DialStageSettings is the session settings autodb re-applies to a freshly
	// acquired backend before handing it to a request.
	DialStageSettings DialStage = "settings"
	// DialStageUnclassified is a failure this package could not attribute to
	// one of the stages above.
	//
	// IT IS A REAL STAGE RATHER THAN A FALLBACK ONTO THE NEAREST PLAUSIBLE
	// ONE. Guessing "connect" for an error nobody recognised would put a wrong
	// fact in the operator's trail, and a wrong fact is worse than an absent
	// one: an operator reading "connect" goes and tests the socket, finds it
	// healthy, and concludes the trail is lying about something else too.
	DialStageUnclassified DialStage = "unclassified"
)

// ErrDialFailed is the sentinel every backend-acquisition failure carries, so
// the renderer can recognise one without knowing any stage.
var ErrDialFailed = errors.New("exec: the backend connection for this request could not be established")

// DialFailure is one request's failed backend acquisition.
//
// IT DELIBERATELY DOES NOT UNWRAP TO ITS CAUSE, and that is the mechanism that
// keeps the cause off the wire rather than a promise that it stays off.
//
// The front door's renderers ask, in order, whether an error is a structured
// admission refusal and whether it is a *pgconn.PgError, and both questions
// are asked with errors.As, which walks the whole chain. An upstream
// authentication failure IS a *pgconn.PgError; wrapping one in an Unwrap chain
// would let it match that arm and be forwarded to the client verbatim, field
// for field, which is precisely the disclosure this type exists to prevent.
// The comment saying "do not forward this" would have been true and the code
// would have forwarded it anyway.
//
// So the cause is reachable only through Cause, which nothing on the wire path
// calls, and errors.Is answers for the sentinel alone.
type DialFailure struct {
	// Stage is which part of opening the connection failed.
	Stage DialStage
	// Attempts is how many acquisitions were made before giving up. It is
	// bounded by dialAttemptsPerRequest and is recorded so an operator reading
	// a trail full of these can tell a target that failed once from one that
	// is being retried in a loop — and so a test can prove the bound holds.
	Attempts int
	cause    error
}

// NewDialFailure builds a dial failure for a cause, classifying its stage.
//
// Exported because the failure is raised where a backend is acquired and
// RENDERED in another package, and the cells that prove the client contract
// have to be able to produce one without an unreachable target of their own.
func NewDialFailure(cause error) *DialFailure {
	return &DialFailure{Stage: dialStageOf(cause), Attempts: 1, cause: cause}
}

// NewDialFailureAt builds one for a caller that already knows the stage,
// because it was performing that stage when the failure happened.
func NewDialFailureAt(stage DialStage, cause error) *DialFailure {
	return &DialFailure{Stage: stage, Attempts: 1, cause: cause}
}

func (d *DialFailure) Error() string {
	return fmt.Sprintf("%s: stage %s after %d attempt(s): %v",
		ErrDialFailed.Error(), d.Stage, d.Attempts, d.cause)
}

// Is answers for the sentinel and for NOTHING ELSE. See the type comment: an
// Unwrap to the cause would hand the cause to the front door's target-error
// arm.
func (d *DialFailure) Is(target error) bool { return target == ErrDialFailed }

// Cause is the raw driver error, for the audit trail only.
func (d *DialFailure) Cause() error { return d.cause }

// AuditDetail is the operator's whole of it: stage, attempts and raw cause, in
// one line an operator can grep.
func (d *DialFailure) AuditDetail() string {
	return fmt.Sprintf("stage=%s attempts=%d cause=%v", d.Stage, d.Attempts, d.cause)
}

// DialFailureOf extracts a dial failure from an error chain.
func DialFailureOf(err error) (*DialFailure, bool) {
	var d *DialFailure
	ok := errors.As(err, &d)
	return d, ok
}

// dialStageOf attributes a driver error to the stage that produced it.
//
// ORDERED FROM THE MOST SPECIFIC ANSWER TO THE LEAST, because the types nest:
// a TLS handshake failure on a dialled socket is reported inside a
// *net.OpError, so testing for the transport first would file every TLS
// failure as a connect failure and an operator would replace a firewall rule
// to fix an expired certificate.
func dialStageOf(err error) DialStage {
	if err == nil {
		return DialStageUnclassified
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return DialStageResolve
	}
	var certInvalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	var unknownAuthority x509.UnknownAuthorityError
	var recordHeader tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &certInvalid) || errors.As(err, &hostErr) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &recordHeader) ||
		errors.As(err, &alert) {
		return DialStageTLS
	}
	// READ THROUGH THE SQLState ACCESSOR rather than off a field. Two reasons,
	// and the second is the one that matters: this package must not reach into
	// a Code field at all — the boundary cell walks the syntax tree for exactly
	// that selector, because a core that starts reading codes is a core that
	// starts deciding policy from them. Asking the error what state it carries
	// works for anything that can answer, not only for one driver's type.
	var stated interface{ SQLState() string }
	if errors.As(err, &stated) {
		// The target answered the startup exchange with a refusal of its own.
		// Class 28 is its authorization classes and 3D000 is "no such
		// database"; anything else it can raise during startup belongs to the
		// startup exchange rather than to the credential.
		state := stated.SQLState()
		if strings.HasPrefix(state, "28") || state == "3D000" {
			return DialStageAuthenticate
		}
		return DialStageStartup
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return DialStageConnect
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DialStageConnect
	}
	return DialStageUnclassified
}

// dialAttemptsPerRequest is ONE grant plus EXACTLY ONE re-arbitration.
//
// The bound is the whole requirement, not a tuning choice. A permit is a
// scarce, instance-wide allowance: a target that always fails to dial would,
// with an unbounded retry, win a permit, lose it, and win it again for as long
// as the request lived — consuming the budget every other session is queued
// for while making no progress. One re-arbitration is there because the first
// failure genuinely can be a permit granted against a backend that has just
// gone away, and a single retry converts that into success; the second failure
// is evidence about the target rather than about the moment, and retrying past
// it buys nothing and costs the budget.
const dialAttemptsPerRequest = 2

// acquireRequestBackend is the REQUEST-SCOPED backend acquisition, and the one
// place a dial failure is raised.
//
// It returns the session's existing backend when it has one, so an established
// session pays nothing here. When it has to acquire, a failure is retried
// exactly once and then becomes a DialFailure carrying the stage and the raw
// cause for the audit — never for the wire.
//
// The retry re-enters the acquisition rather than looping over the dial itself,
// deliberately: the permit is taken and released inside the driver's dialer, so
// a second acquisition is a second arbitration for a permit, which is what the
// bound is about. Looping inside one acquisition would hold the permit across
// both attempts and prove nothing.
func (e *Engine) acquireRequestBackend(ctx context.Context, s *session,
	connRow *meta.Connection) (golibpg.PinnedConn, error) {

	return acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		return e.pinWireSession(ctx, s, connRow)
	})
}

// acquireWithReArbitration applies the bound to any acquisition.
//
// SEPARATE FROM THE ENGINE METHOD so the bound itself is provable. A cell that
// had to stand up a pool, a target and a session to count attempts would be
// measuring three things at once, and the one it is about — that a permanently
// failing target costs exactly two arbitrations and not a loop of them — is the
// one such a cell proves least directly.
func acquireWithReArbitration(ctx context.Context,
	attempt func() (golibpg.PinnedConn, error)) (golibpg.PinnedConn, error) {

	var last error
	made := 0
	for made < dialAttemptsPerRequest {
		made++
		pc, err := attempt()
		if err == nil {
			return pc, nil
		}
		last = err
		// A CANCELLED OR EXPIRED REQUEST IS NOT A FAILING TARGET. Retrying one
		// would spend a second permit on work the caller has already stopped
		// waiting for, and would report the target as unreachable when the only
		// thing that ended was the request.
		if ctx.Err() != nil {
			break
		}
	}
	if d, ok := DialFailureOf(last); ok {
		d.Attempts = made
		return nil, d
	}
	f := NewDialFailure(last)
	f.Attempts = made
	return nil, f
}
