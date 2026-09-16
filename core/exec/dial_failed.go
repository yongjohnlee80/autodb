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

	"github.com/yongjohnlee80/autodb/core/auth"
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
	// EVERY FIELD IS PRIVATE, AND THAT IS A CORRECTION RATHER THAN A STYLE.
	//
	// Stage and Attempts were exported, which made two things possible that
	// should not have been. A caller outside this package could assign
	// DialStage("<anything>") into Stage -- the type is a defined string, so
	// the conversion compiles -- and that text then appeared in Error and in
	// the audit row. And any holder could mutate a failure after it was
	// raised, so what the audit recorded need not be what the raise site
	// decided. Accessors below give every legitimate reader what it needs.
	stage    DialStage
	attempts int
	// connID is the connection's opaque numeric id. Safe to publish: a row
	// number an operator looks up, never a name, a host or a credential.
	connID int64
	cause  error
}

// Stage is which part of opening the connection failed.
func (d *DialFailure) Stage() DialStage { return d.stage }

// Attempts is how many acquisitions were made before giving up.
func (d *DialFailure) Attempts() int { return d.attempts }

// ConnID is the connection this failure is about, or zero when the raise site
// did not know it.
func (d *DialFailure) ConnID() int64 { return d.connID }

// dialStages is every stage this package may record, for the normalization
// below and for the walk that proves nothing else reaches a projection.
func dialStages() []DialStage {
	return []DialStage{DialStageResolve, DialStageConnect, DialStageTLS,
		DialStageAuthenticate, DialStageStartup, DialStageSettings, DialStageUnclassified}
}

// normalizeStage maps anything not declared here onto the unclassified stage.
//
// AN UNKNOWN VALUE IS NOT PASSED THROUGH, because passing it through is how a
// projection of "fixed values only" stops being one: a caller that can get an
// arbitrary string into the field can get it onto the wire's DETAIL and into
// the operator's trail. Unclassified is a real stage with a real meaning --
// "this package could not attribute it" -- which is exactly true of a value
// this package does not recognise.
func normalizeStage(st DialStage) DialStage {
	for _, known := range dialStages() {
		if st == known {
			return st
		}
	}
	return DialStageUnclassified
}

// NewDialFailure builds a dial failure for a cause, classifying its stage.
//
// Exported because the failure is raised where a backend is acquired and
// RENDERED in another package, and the cells that prove the client contract
// have to be able to produce one without an unreachable target of their own.
func NewDialFailure(cause error) *DialFailure {
	return &DialFailure{stage: normalizeStage(dialStageOf(cause)), attempts: 1, cause: cause}
}

// ForConnection records which connection this failure is about. Returns the
// same failure so a raise site can chain it onto a constructor.
func (d *DialFailure) ForConnection(connID int64) *DialFailure {
	d.connID = connID
	return d
}

// NewDialFailureAt builds one for a caller that already knows the stage,
// because it was performing that stage when the failure happened.
func NewDialFailureAt(stage DialStage, cause error) *DialFailure {
	return &DialFailure{stage: normalizeStage(stage), attempts: 1, cause: cause}
}

// Error projects the fixed values and NOT the cause.
//
// THE CAUSE USED TO BE HERE, AND IT IS THE SAME DEFECT THE CONFIGURATION
// FAILURE BESIDE THIS ONE WAS CORRECTED FOR -- found there, fixed there, and
// left standing here because this type was the model the fix was copied FROM.
// A driver's connect error is shaped "failed to connect to host=... user=...
// database=... password=..." and, for a URL DSN, carries the whole connection
// string; measured, this projection reproduced the host, the role, the
// database, a plaintext password, a PAT and a query-parameter secret.
//
// This text reaches operator logs through paths that do not distinguish audit
// from disclosure, and it is the same string the audit row publishes. The
// stage says which component broke, which is what an operator acts on; the
// cause is reachable in-process through Cause and nowhere else.
func (d *DialFailure) Error() string {
	return fmt.Sprintf("%s: stage %s after %d attempt(s)",
		ErrDialFailed.Error(), d.stage, d.attempts)
}

// Is answers for the sentinel and for NOTHING ELSE. See the type comment: an
// Unwrap to the cause would hand the cause to the front door's target-error
// arm.
func (d *DialFailure) Is(target error) bool { return target == ErrDialFailed }

// Cause is the raw driver error, for the audit trail only.
func (d *DialFailure) Cause() error { return d.cause }

// AuditDetail is the operator's whole of it: stage, attempts and the
// connection's opaque id, in one line an operator can grep.
//
// NO CAUSE. This string becomes EventDialFailed.Detail, which is published to
// whatever consumes the event stream -- wider than the operator's terminal,
// and wide enough that a driver's connect error appearing here is a
// credential and topology disclosure rather than a debugging convenience.
func (d *DialFailure) AuditDetail() string {
	return fmt.Sprintf("stage=%s attempts=%d conn=%d", d.stage, d.attempts, d.connID)
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
// place a dial failure is raised for a request.
//
// It returns the session's existing backend when it has one, so an established
// session pays nothing here. When it has to acquire, the target pool is
// resolved FIRST and only the acquisition proper — taking a member and proving
// that member clean — is retried, exactly once, before becoming a DialFailure
// carrying the stage and the raw cause for the audit and never for the wire.
//
// THE TARGET MUST BE RESOLVED OUTSIDE THE RETRY BOUND, so that an attempt means
// one thing: one arbitration for one permit. Resolving the pool answers
// questions that have nothing to do with a permit — whether the connection is
// being deleted, whether the secret store has been unlocked, whether this
// process already holds a pool for it. What goes wrong when those sit inside
// the bound is that a settled answer is asked for twice and then reported as a
// target outage: an operator reading "2 attempts" is told a backend was
// arbitrated for twice when no permit was ever sought, and a deleted connection
// is reported as an unreachable database.
//
// THE OBVIOUS ALTERNATIVE — wrapping the whole pin in the bound because it is
// one function call and reads as one step — is what this replaced, and the
// reason it is wrong is that the bound exists to ration a scarce instance-wide
// allowance. A bound that counts work which never touched the allowance is not
// rationing anything.
//
// The retry re-enters the acquisition rather than looping over the dial itself,
// deliberately: the permit is taken and released inside the driver's dialer, so
// a second acquisition is a second arbitration for a permit, which is what the
// bound is about. Looping inside one acquisition would hold the permit across
// both attempts and prove nothing.
func (e *Engine) acquireRequestBackend(ctx context.Context, s *session,
	connRow *meta.Connection) (golibpg.PinnedConn, error) {

	if pc := s.pinnedConn(); pc != nil {
		return pc, nil
	}
	target, terr := e.target(ctx, connRow.ID, connRow)
	if terr != nil {
		return nil, requestTargetFailure(ctx, terr)
	}
	return acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		return e.pinTargetBackend(ctx, s, target)
	})
}

// requestTargetFailure decides what a failure to resolve the target pool IS.
//
// THREE ANSWERS, AND ONLY THE THIRD IS A DIAL FAILURE. They are told apart by
// who has to act on them and whether anything physical happened.
//
// THE CALLER GAVE UP. A context that is already done when target resolution
// fails means the request was abandoned — cancelled, or past its deadline —
// and the error the resolution produced is a consequence of that, not a fact
// about the target. The caller's own cause is returned, exactly as the pin
// loop does it. This was previously only half right: context.Canceled was
// listed and passed through, but a DEADLINE that expired inside resolution
// was not, so an abandoned request was reported to operators as a target
// outage. Asking the context rather than listing sentinels is what makes the
// two cases the same case.
//
// AUTODB'S OWN ANSWERS ABOUT A CONNECTION MUST REACH THE CALLER CARRYING THEIR
// OWN SENTINEL, because every one of them is actionable and a dial failure is
// deliberately not: the client shape for a dial failure is one fixed literal
// that names nothing, so folding "this connection is being deleted" or "the
// secret store is still locked" into it replaces a fact the operator can act
// on with a uniform "the target could not be reached". They are listed rather
// than inferred because there is nothing in the error itself that
// distinguishes them. A configuration failure is already typed by the site
// that raised it and needs no listing here.
//
// EVERYTHING ELSE HERE CAME OUT OF THE DRIVER AND MUST BE FRAMED, and that is
// why this is not simply a passthrough. Opening the pool decrypts a DSN and
// hands it to the driver, and the error that comes back is wrapped with the
// connection's name and the driver's own text — target host, port, database,
// and the role autodb connects as. The front door's refusal renderer has a
// default arm that puts an unrecognised error's text on the wire, so returning
// that error unframed would publish the install's topology to anyone holding a
// socket, which is the exact disclosure this file exists to prevent.
func requestTargetFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return requestAbandoned(ctx)
	}
	if _, ok := ConfigFailureOf(err); ok {
		return err
	}
	for _, own := range []error{ErrConnectionDraining, auth.ErrLocked, context.Canceled} {
		if errors.Is(err, own) {
			return err
		}
	}
	return NewDialFailure(err)
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
		// A CANCELLED OR EXPIRED REQUEST IS NOT A FAILING TARGET, AND IT MUST
		// LEAVE THIS FUNCTION AS THE CANCELLATION IT IS.
		//
		// Two things go wrong if it does not. Retrying spends a second permit
		// — an instance-wide allowance other sessions are queued for — on work
		// the caller has already stopped waiting for. Reporting it as a dial
		// failure is the worse half: a dial failure is rendered to the client
		// as a target outage and written to the operator's trail as one, so a
		// client that pressed Ctrl-C manufactures evidence that the database is
		// unreachable, and somebody goes looking for a network fault that never
		// existed.
		//
		// THE OBVIOUS ALTERNATIVE — stopping the retry here and letting the
		// wrap below run anyway — is what this replaced. It fixes the permit
		// half and leaves the false outage, which is the half the operator
		// actually reads.
		if ctx.Err() != nil {
			return nil, requestAbandoned(ctx)
		}
		// A CONNECTION THIS INSTALL CANNOT SERVE IS NOT A TARGET TO TRY AGAIN.
		//
		// This is where the separation was being undone. The classification at
		// target resolution was correct and the check at the pin boundary was
		// correct, and then this loop retried the failure and wrapped whatever
		// came back, so a capability or configuration fault reached the caller
		// as DialFailed(unclassified, attempts=2): a target outage in the
		// operator's trail, a fabricated acquisition count, the capability
		// stage gone, and a SECOND pin attempted against a driver already
		// known to be unusable.
		//
		// A HELPER THAT ANSWERS CORRECTLY AND A CALLER THAT OVERRIDES IT IS
		// THE SAME AS A HELPER THAT ANSWERS WRONGLY, and the cell that proved
		// the boundary was calling pinTargetBackend directly, so it could not
		// see its own caller. That is why the acceptance cell for this drives
		// acquireRequestBackend instead.
		if _, ok := ConfigFailureOf(err); ok {
			return nil, err
		}
	}
	if d, ok := DialFailureOf(last); ok {
		d.attempts = made
		return nil, d
	}
	f := NewDialFailure(last)
	f.attempts = made
	return nil, f
}

// requestAbandoned is why the request ended, for a caller that stopped waiting.
//
// IT PREFERS THE CAUSE A CALLER ATTACHED over the bare sentinel, because a
// front door that cancels a request for a reason of its own — a lease that
// expired, a session that was closed underneath the statement — has already
// written that reason down, and returning context.Canceled instead would throw
// away the only description of what actually happened. A context cancelled
// without a cause reports the sentinel, which is what context.Cause does on its
// own; the explicit fallback is there for a context whose Done is closed but
// which is not one this package derived.
func requestAbandoned(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}
