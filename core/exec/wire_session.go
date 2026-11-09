package exec

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The front door's authentication chain (protocol matrix row 2.7).
//
// Every check below runs, then ONE atomic reservation. The order is the
// matrix's and it is not arbitrary: the cheap, purely local checks come
// before the queries, and the target is validated before any capacity is
// taken, so a connection that was never going to be served does not briefly
// hold a lease.
//
// WHICH CHECK FAILED IS AUDIT-ONLY. The wire gets one shape for all of them.
// A caller who could distinguish "no such database" from "no grant" from
// "wrong token" could map the install without ever holding a credential —
// slowly, but nobody notices slow.

// WireSessionResult is what a successful authentication produced.
type WireSessionResult struct {
	SessionID SessionID
	UserID    int64
	ConnID    int64
	// AdmissionSource records WHICH allowlist layer admitted the address
	// Audited, so an operator can tell a connection
	// from shared infrastructure apart from one from a person's own
	// registered address.
	AdmissionSource auth.AdmissionSource
	// PATName is the credential that authenticated, for the audit trail:
	// who, under whose account, with which token.
	PATName string
	// ParameterStatuses is every ParameterStatus the pinned target connection
	// reported at its own connect (matrix §3.3): the loop forwards them verbatim
	// at session open, overriding only is_superuser and application_name. Empty
	// for non-postgres targets.
	ParameterStatuses map[string]string
	// ApplicationName is the client's accepted startup application_name, as
	// recorded on the session (matrix claim 3.1:application_name#session-audit).
	ApplicationName string
	// UserName is the CANONICAL owner name, not the client's spelling of
	// it. The startup `user` parameter matched it case-insensitively (step
	// 2), so echoing the parameter back as session_authorization would let
	// the client's casing become the session's identity — and the identity
	// a session reports should be the one the grants are written against.
	UserName string
}

// wireDenial carries the INTERNAL reason for an audit row. The caller turns
// every one of these into the same wire denial.
type wireDenial struct {
	reason string
	// authorized records that the credential had already verified when this
	// refusal was raised. It is set by denyAfterAuthorization and by nothing
	// else, so it cannot be claimed by a path that has not earned it.
	authorized bool
	// detail is bounded operator-facing diagnosis that travels WITH the
	// denial rather than beside it.
	//
	// IT EXISTS SO THERE IS ONE RECORD AND NOT TWO. The first version of the
	// expired-wait path wrote its own audit line for the blocking cap and then
	// returned the denial, so a single refusal produced a side audit AND the
	// registered occurrence -- two rows for one event, which is exactly what a
	// single-occurrence rule exists to prevent, and which leaves anyone
	// counting refusals double-counting them. Carried here, the diagnosis
	// reaches the one occurrence that was always going to be written.
	//
	// It never reaches the wire: the client is told the uniform thing its
	// reason maps to, and this is for the trail alone.
	detail string
}

func (e wireDenial) Error() string { return "frontdoor: " + e.reason }

// DenialReason extracts the internal reason from an OpenWireSession error, or
// "" if the error is not a denial (a store failure, say, which must not be
// reported as an authentication problem).
func DenialReason(err error) string {
	var d wireDenial
	if errors.As(err, &d) {
		return d.reason
	}
	return ""
}

func deny(reason string) error { return wireDenial{reason: reason} }

// denyAfterAuthorization builds a denial that CARRIES PROOF the caller was
// already authorized when it was refused.
//
// THE WITNESS IS THE MECHANISM, not a label. A refusal may disclose "the
// system is full" only to somebody who has already proved who they are --
// telling an unauthenticated stranger that would hand them a capacity oracle.
// Deciding that from the REASON would mean a table mapping reasons to wire
// codes, and a table is how a uniform surface becomes an enumerable one, one
// well-meaning row at a time.
//
// Carrying it in the value instead makes the exemption survive a reordering BY
// CONSTRUCTION: move a capacity check above the credential check and there is
// no authorized denial to build, so the wire silently stays uniform rather
// than leaking. It can only be called from a site that has the verified
// credential in hand.
func denyAfterAuthorization(reason string) error {
	return wireDenial{reason: reason, authorized: true}
}

// denyAfterAuthorizationWithDetail is denyAfterAuthorization plus bounded
// operator diagnosis for the one occurrence this denial becomes.
func denyAfterAuthorizationWithDetail(reason, detail string) error {
	return wireDenial{reason: reason, authorized: true, detail: detail}
}

// DenialDetail extracts the operator-facing diagnosis carried by a denial, or
// "" when it carries none. Paired with DenialReason for the same reason: the
// package that owns the vocabulary owns how it is read.
func DenialDetail(err error) string {
	var d wireDenial
	if errors.As(err, &d) {
		return d.detail
	}
	return ""
}

// DenialDisclosable reports whether this denial may say what it was.
//
// True only for a refusal raised after the credential verified. A caller uses
// it to choose a specific wire code; false means the uniform denial, which is
// what every pre-authorization refusal gets.
func DenialDisclosable(err error) bool {
	var d wireDenial
	return errors.As(err, &d) && d.authorized
}

// WireDenial builds a denial with the given internal reason.
//
// Exported as the pair of DenialReason: the package that owns the reason
// vocabulary owns how one is constructed, so a caller in another package
// cannot invent a denial shape this package would not recognise. The front
// door's cells build one to prove the wire treats every reason identically.
func WireDenial(reason string) error { return deny(reason) }

// The internal denial reasons. Each names a distinct operator-visible cause;
// none of them ever reaches the wire.
const (
	DenyBadCredential  = "frontdoor/bad-credential"
	DenyUserMismatch   = "frontdoor/startup-user-mismatch"
	DenyIPNotAdmitted  = "frontdoor/ip-not-admitted"
	DenyPATIPNarrowed  = "frontdoor/pat-allowed-ips"
	DenyNoSuchDatabase = "frontdoor/no-such-database"
	// DenyPATUnscoped: the presented token carries conn_id = 0 — a pre-v13
	// token, or a v13 tombstone somebody un-revoked by hand.
	// Refused INDEPENDENTLY of `revoked`, so re-flipping that column cannot
	// bring an unscoped credential back.
	DenyPATUnscoped = "frontdoor/pat-unscoped"
	// DenyDatabaseMismatch: the startup `database` agrees with neither the
	// bound connection's name, nor its target_db, nor conn:<id>.
	//
	// NEVER silently substituted: a client that asked for `postgres` and was
	// quietly given `test` is worse than any refusal.
	DenyDatabaseMismatch = "frontdoor/database-mismatch"
	// DenyPATNotCleartextDebug: a cleartext listener met an ordinary token
	// (the cleartext debugging mode). Not charged — the credential is valid and the peer may be
	// fully admitted; what refuses it is our mode.
	DenyPATNotCleartextDebug = "frontdoor/pat-not-cleartext-debug"
	// DenyPATCleartextDebugInTLS: a TLS listener met a debug_cleartext token.
	// The exact mirror of the above, and uncharged for the same reason — the
	// token is perfectly valid on the listener it was minted for.
	DenyPATCleartextDebugInTLS = "frontdoor/pat-cleartext-debug-in-tls"
	DenyNoGrant                = "frontdoor/no-grant"
	// DenyProfileRefuses retains its published identity for compatibility;
	// the explicit exposure property now drives this pre-disclosure refusal.
	DenyProfileRefuses = "frontdoor/profile-not-front-door"
	DenyLeaseCap       = "frontdoor/lease-cap-exceeded"
	DenySessionCap     = "frontdoor/session-cap-exceeded"
	DenyResidentBudget = "frontdoor/resident-budget-exceeded"
	// The four below all mean the request WAITED. They are separate identities
	// from the caps above, and separate from each other, because an operator
	// reading the trail has to be able to tell apart four different stories
	// that a single "too many connections" would flatten into one: the
	// instance was full for ninety seconds; the instance had nothing coming so
	// did not make anyone wait; the target was removed out from under a
	// waiting request; and the instance began shutting down. The first is
	// pressure and warrants more capacity, the second is transaction
	// behaviour, and the last two are operator actions.
	DenyQueueTimeout = "frontdoor/queue-timeout"
	// DenyAllCapacityInTransaction is the pre-enqueue refusal: every lease on
	// the target is held by a transaction still inside its bounds, so nothing
	// is going to be released and waiting would end in the same answer.
	DenyAllCapacityInTransaction = "frontdoor/all-capacity-in-transaction"
	DenyTargetGone               = "frontdoor/target-removed-while-waiting"
	// DenyConnectionChanging: the connection is being moved to another pool
	// or downgraded; it is still there, and a retry succeeds once it lands.
	DenyConnectionChanging = "frontdoor/connection-changing"
	// DenyTargetBudget: the session was admitted, but its pin waited for the
	// pool's front-door share until its bound (a closing backend's socket was
	// still in flight).
	DenyTargetBudget  = "frontdoor/target-budget-exhausted"
	DenyEngineClosing = "frontdoor/instance-shutting-down"
	// DenyLeaseEncoding: the pinned target's server_encoding or client_encoding
	// is not UTF8, or could not be established (matrix row 3.1: the lease is
	// pinned UTF8; autodb does not transcode; the check FAILS CLOSED). On the
	// wire it is the uniform 28000 like every lease failure in the reservation
	// phase (§7 ruling 4); this reason is the audit identity only.
	DenyLeaseEncoding = "frontdoor/lease-encoding"
	// DenyStartupGUC: a startup parameter named a setting this session may not
	// change, or the target refused to apply it.
	// Uniform 28000 on the wire (matrix §7 ruling 4); the audit row names the key.
	//
	// IT USED TO SHARE ITS VALUE WITH THE FRONT DOOR'S PRE-VERIFICATION
	// STARTUP REFUSAL, and the comment here asserted they were different
	// identities while the string said they were one. They are not the same
	// thing and they do not cost the same: the front door's fires before any
	// credential and is charged as protocol grinding, while this one fires
	// only after a PAT has been verified and is a configuration mistake
	// nobody should be banned for.
	//
	// So an operator counting one string was counting both, and could not
	// tell a peer probing the startup surface from a developer whose client
	// sets a GUC the session may not. Two meanings, two charges, one
	// identity — split here, keeping the pre-verification value unchanged
	// because it is the one with the history in existing audit trails.
	DenyStartupGUC = "frontdoor/startup-guc-refused"
)

// ChargeClass says who a denial is ABOUT, which decides whether it counts
// against the credential throttle.
//
// The distinction is the whole of the charge-class policy. A front door that counts "we are
// full" as "you guessed wrong" bans the developer it just refused for
// capacity — which is exactly what happened on 2026-09-15, when a pool that
// granted four leases filed the fifth connection as a failed login and the
// tenth banned the source address.
type ChargeClass uint8

const (
	// ChargeCredential: the caller presented something wrong. Charges.
	ChargeCredential ChargeClass = iota
	// ChargeProtocol: the caller's frames were malformed or disallowed. Charges.
	ChargeProtocol
	// ChargeCapacity: we ran out of something. NEVER charges — a queue is not
	// evidence about a credential.
	ChargeCapacity
	// ChargeNone: our own fault — our store, our config, our bug. NEVER
	// charges.
	ChargeNone
)

// Charges reports whether this class counts against the credential throttle.
func (c ChargeClass) Charges() bool { return c == ChargeCredential || c == ChargeProtocol }

func (c ChargeClass) String() string {
	switch c {
	case ChargeCredential:
		return "credential"
	case ChargeProtocol:
		return "protocol"
	case ChargeCapacity:
		return "capacity"
	case ChargeNone:
		return "none"
	}
	return "unknown"
}

// denialCharge is the registry: every post-verification denial reason, with
// the class the policy ruled for it (see docs/front-door/connection-holding-policy.md).
//
// A MAP RATHER THAN A SWITCH WITH A DEFAULT, deliberately. The old switch
// charged anything it had not been taught about, so eight reasons were
// mis-charged silently and a ninth would have joined them unnoticed. Here a
// new reason is ABSENT rather than wrong, and TestEveryDenialReasonHasACharge
// fails the build until somebody classifies it.
var denialCharge = map[string]ChargeClass{
	// Credential — the caller presented something wrong. Unchanged.
	DenyBadCredential: ChargeCredential,
	DenyUserMismatch:  ChargeCredential,
	DenyIPNotAdmitted: ChargeCredential,
	DenyPATIPNarrowed: ChargeCredential,

	// Capacity — we ran out. Ruled uncharged: see docs/front-door/connection-holding-policy.md.
	DenyLeaseCap:       ChargeCapacity,
	DenySessionCap:     ChargeCapacity,
	DenyResidentBudget: ChargeCapacity,
	// Waiting and not being served is the same KIND of answer as being refused
	// on arrival: the caller presented nothing wrong, so none of these may
	// touch the per-source throttle. Charging them would be worse than
	// charging an ordinary capacity refusal -- a developer who queued politely
	// for ninety seconds would be banned for their patience.
	DenyQueueTimeout:             ChargeCapacity,
	DenyAllCapacityInTransaction: ChargeCapacity,
	DenyTargetGone:               ChargeCapacity,
	DenyConnectionChanging:       ChargeCapacity,
	DenyTargetBudget:             ChargeCapacity,
	DenyEngineClosing:            ChargeCapacity,

	// None — our config, our stored state, our bug. Ruled uncharged: see docs/front-door/connection-holding-policy.md.
	// The last three each follow a VERIFIED PAT bound to the exact connection:
	// the caller has already proved who they are, and what they met is a fact
	// about what we stored. Charging them means a developer whose grant was
	// never set up bans themselves by retrying.
	DenyProfileRefuses: ChargeNone,
	DenyLeaseEncoding:  ChargeNone,
	DenyNoSuchDatabase: ChargeNone,
	DenyNoGrant:        ChargeNone,
	DenyPATUnscoped:    ChargeNone,

	// Mode-side, and already uncharged before that ruling — the token is valid on
	// the listener it was minted for.
	DenyPATNotCleartextDebug:   ChargeNone,
	DenyPATCleartextDebugInTLS: ChargeNone,

	// Client misconfiguration, not an attack: an introspecting client dials
	// every database it discovered. Ruled by Johno, 2026-09-05.
	DenyDatabaseMismatch: ChargeNone,

	// POST-verification startup GUC. The pre-verification startup-policy
	// refusal is a different identity and stays Protocol; this constant only
	// ever names the post-verification outcome, which is after a verified PAT
	// and is therefore a configuration mistake rather than credential
	// grinding (see docs/front-door/connection-holding-policy.md).
	DenyStartupGUC: ChargeNone,
}

// DenialCharge returns the ruled class for a denial reason.
//
// The second result is false for a reason with no registered class. A caller
// must fail SAFE — charge it — because an unclassified reason is one nobody
// has reasoned about; but the exhaustiveness test means this cannot happen to
// a reason declared in this file.
func DenialCharge(reason string) (ChargeClass, bool) {
	// THROUGH THE REGISTRY, not the map beside it. The map is the
	// DECLARATION; this is the lookup, and routing it through the composed
	// registry is what makes the two one fact rather than two facts that
	// happen to agree today. An identity the registry never heard of is
	// refused here exactly as it would be at any other producer's emit site.
	d, ok := engineRegistry.Lookup(outcome.ReasonID(reason))
	if !ok {
		return 0, false
	}
	return classOf(d.Charge)
}

// DenialReasons lists every declared post-verification denial reason, so the
// exhaustiveness test can walk them.
func DenialReasons() []string {
	return []string{
		DenyBadCredential, DenyUserMismatch, DenyIPNotAdmitted, DenyPATIPNarrowed,
		DenyNoSuchDatabase, DenyPATUnscoped, DenyDatabaseMismatch,
		DenyPATNotCleartextDebug, DenyPATCleartextDebugInTLS, DenyNoGrant,
		DenyProfileRefuses, DenyLeaseCap, DenySessionCap, DenyResidentBudget,
		DenyLeaseEncoding, DenyStartupGUC,
		DenyQueueTimeout, DenyAllCapacityInTransaction, DenyTargetGone, DenyEngineClosing,
		DenyConnectionChanging, DenyTargetBudget,
	}
}

// AuthorizeDenials is every denial AuthorizeWire can return, and AdmitDenials
// every one Admit can: the front door's two phases each declare exactly the set
// their engine call can raise, so the registry can answer "what can happen in
// this phase" and refuse a wiring mistake. The sets overlap where
// finalization re-runs authorization's connection checks after the wait.
// TestWireDenials_EachHalfDeclaresExactlyWhatItRaises derives both from the
// source and fails on any difference.
func AuthorizeDenials() []string {
	return []string{
		DenyBadCredential, DenyUserMismatch, DenyPATNotCleartextDebug, DenyPATCleartextDebugInTLS,
		DenyIPNotAdmitted, DenyPATIPNarrowed, DenyPATUnscoped,
		DenyNoSuchDatabase, DenyNoGrant, DenyProfileRefuses, DenyDatabaseMismatch,
	}
}

// AdmitDenials: see AuthorizeDenials.
func AdmitDenials() []string {
	return []string{
		// The fresh connection read and standing re-check after the wait.
		DenyNoSuchDatabase, DenyNoGrant, DenyProfileRefuses, DenyDatabaseMismatch,
		// The line and the pin (admissionAnswer).
		DenyQueueTimeout, DenyLeaseCap, DenyAllCapacityInTransaction, DenyTargetGone,
		DenyConnectionChanging, DenyTargetBudget, DenyEngineClosing, DenySessionCap, DenyResidentBudget,
		// The pinned backend.
		DenyLeaseEncoding, DenyStartupGUC,
	}
}

// admissionAnswer names the answer an admission refusal earns, WITHOUT
// claiming anything about who the caller is.
//
// IT RETURNS A REASON, NOT A DENIAL, AND THAT DISTINCTION IS THE POINT. The
// first version of this returned a finished denial built with
// denyAfterAuthorization, which stamps a refusal as disclosable — safe to tell
// the caller what actually happened rather than handing them the uniform
// credential answer. A guard caught it immediately: that stamp was being
// applied inside a function which never verifies a credential or checks a
// grant, so the claim rested on a comment saying the caller had done it. "By
// construction" is exactly what that guard exists to refuse, and it was right
// to: a capacity check moved above either gate would have kept the stamp and
// started telling unauthenticated callers how full the instance is.
//
// So the authorization stays where it is established. This says which refusal
// it is; the caller, which has the verified credential and the checked grant in
// hand, is what turns that into something disclosable.
//
// The ORDER remains the contract, and it is testable here precisely because
// this is reachable without the rest of a session open — see
// TestAdmissionDenial_TheWaitOutranksTheCapItWaitedOn.
func admissionAnswer(rerr error) (reason, detail string, ok bool) {
	switch {
	// THE WAIT IS TESTED BEFORE THE CAPS. A request that waited must not be
	// recorded as one refused on arrival, so the identity that says it waited
	// has to win before any cap arm can claim it — even for an error that
	// satisfies both.
	case errors.Is(rerr, ErrQueueTimeout):
		// THE BLOCKING CAP TRAVELS WITH THE DENIAL, NOT BESIDE IT. A second
		// audit row would double-count one refusal: the denial the caller
		// builds already becomes a registered occurrence, and the operator
		// needs one record saying both what happened and which limit to raise.
		d := "waited and was not served"
		var waited *QueueTimeoutError
		if errors.As(rerr, &waited) {
			d = fmt.Sprintf("waited %s and was not served", waited.Waited())
		}
		var qt *QueueTimeoutError
		if errors.As(rerr, &qt) && qt.BlockedBy() != nil {
			d += "; blocked by: " + qt.BlockedBy().Error()
		}
		return DenyQueueTimeout, d, true
	case errors.Is(rerr, ErrLeaseCapExceeded):
		return DenyLeaseCap, "", true
	case errors.Is(rerr, ErrAllCapacityInTransaction):
		return DenyAllCapacityInTransaction, "", true
	case errors.Is(rerr, ErrTargetGone):
		return DenyTargetGone, "", true
	case errors.Is(rerr, ErrConnectionDraining):
		return DenyConnectionChanging, "", true
	// The pin's own capacity refusal: its share stayed full until its bound.
	// ErrBackendOpenTimeout is deliberately NOT here: a target that doesn't
	// open in time is unavailable, not full (frontdoor startupFailure).
	case errors.Is(rerr, ErrTargetBudgetExhausted):
		return DenyTargetBudget, "", true
	case errors.Is(rerr, ErrEngineClosing):
		return DenyEngineClosing, "", true
	case errors.Is(rerr, ErrSessionCapExceeded):
		return DenySessionCap, "", true
	case errors.Is(rerr, ErrResidentBudgetExceeded):
		return DenyResidentBudget, "", true
	}
	return "", "", false
}

// WireSessionOverhead is the fixed memory charged for one wire session: the
// ExecSession's own state — the session record, its registry and per-user
// entries, the reservation itself, and the bookkeeping the engine keeps for
// the session's whole life.
//
// IT DOES NOT COVER ANY WIRE-SIDE BUFFER, and the first version of this
// comment said it did — "its decoder, TLS record buffers and session
// bookkeeping". That was wrong and it was load-bearing wrong: the front door
// separately reserves 64 KiB per connection for its control lane, whose
// comment also mentions the decoder, so the two read as one charge taken
// twice. They are different terms of the worst case, charged
// against different budgets by different packages. The protocol matrix §8.5
// carries the allocation-to-charge map; review found the drift.
//
// A flat figure rather than a measurement, deliberately, and a review
// ruling accepts it as such: the budget's job is to bound the total, and a
// charge that varied with actual allocation would let a session grow past
// what it reserved — the reservation would stop meaning anything at the
// moment it was most needed. Variable input, retained and output allocations
// stay separately charged where they are made.
const WireSessionOverhead = 64 * 1024

// OpenWireSession authenticates a front-door connection and reserves its
// capacity, returning the session it may then use.
//
// presented is the PAT from the PasswordMessage; startupUser and database are
// the StartupMessage parameters; ip is the canonical peer address.
// WireOpen is everything a front-door session open needs from the startup
// exchange. ApplicationName is the CLIENT's label, already length-capped by the
// front door; it is recorded on the session and in its audit lines.
type WireOpen struct {
	PAT, StartupUser, Database, IP, ApplicationName string
	// StartupGUCs are the StartupMessage parameters outside row 3.1's named
	// set — the settings a client asks for at connect (lib/pq's datestyle,
	// JDBC's TimeZone and extra_float_digits). Each is admitted EXACTLY as
	// `SET name TO value` from this session would be (under the amended rule,
	// one admission implementation) and, when admitted, applied to the pinned
	// backend before the result returns — PostgreSQL's own semantics, not an
	// emulation. One refusal withdraws the session (DenyStartupGUC).
	StartupGUCs map[string]string
	// Cleartext reports that the LISTENER is serving without TLS
	// matrix §10). It comes from the listener rather than from config because it is a
	// property of the connection being opened, and because the engine must not
	// have to re-derive a fact the caller already knows.
	Cleartext bool
}

// OpenWireSession is the original entry point, kept so the front door's
// interface keeps compiling; it opens with no application_name.
func (e *Engine) OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (WireSessionResult, error) {
	return e.OpenWireSessionWith(ctx, WireOpen{PAT: presented, StartupUser: startupUser, Database: database, IP: ip})
}

// OpenWireSessionWith authorizes and admits a front-door client under ONE
// context: AuthorizeWire, then Admit. The front door calls the two halves
// itself, so that the wait in line runs after its credential phase has ended;
// this composition is for callers with a single deadline.
func (e *Engine) OpenWireSessionWith(ctx context.Context, req WireOpen) (WireSessionResult, error) {
	a, err := e.AuthorizeWire(ctx, req)
	if err != nil {
		return WireSessionResult{}, err
	}
	return a.Admit(ctx)
}

// WireAuthorization is a front-door login that passed every credential and
// authorization check, as of the moment it was checked. It holds no lease, no
// lock and no backend; admitWire consumes it once.
type WireAuthorization struct {
	pat   *meta.PAT
	owner *meta.User
	// connRow is the connection as authorized. It names the pool the login waits
	// in; finalization re-reads it fresh, because exposure, the grant and the
	// target may all change during the wait.
	connRow *meta.Connection
	src     auth.AdmissionSource
	req     WireOpen
	// admitter is what Admit runs: the engine's admitWire for this authorization,
	// or a decided result for a PreAdmitted one.
	admitter func(ctx context.Context) (WireSessionResult, error)
}

// Admit waits in the pool's line and finalizes this login (admitWire), under the
// caller's admission context. It is how the front door, which holds only the
// authorization, reaches the engine that issued it.
func (a *WireAuthorization) Admit(ctx context.Context) (WireSessionResult, error) {
	return a.admitter(ctx)
}

// PreAdmitted is an authorization whose admission is already decided: Admit
// returns what admit returns. It exists for an authenticator that settles the
// whole open at authorization time -- a test double standing in for the engine
// -- so the front door keeps one path for both.
func PreAdmitted(admit func(ctx context.Context) (WireSessionResult, error)) *WireAuthorization {
	return &WireAuthorization{admitter: admit}
}

// AuthorizeWire runs a front-door login's credential and authorization checks.
// It reserves nothing and takes no lock: everything a wait could change is
// checked again, fresh, after the wait (finalizeWire).
func (e *Engine) AuthorizeWire(ctx context.Context, req WireOpen) (*WireAuthorization, error) {
	presented, startupUser, database, ip := req.PAT, req.StartupUser, req.Database, req.IP

	// 1. The credential. VerifyPAT already collapses unknown/wrong/revoked/
	// expired/disabled-owner into one error and does comparable work across
	// the paths an attacker can produce.
	pat, err := e.auth.VerifyPAT(ctx, presented)
	if err != nil {
		if errors.Is(err, auth.ErrPATInvalid) {
			return nil, deny(DenyBadCredential)
		}
		// A store failure is not a bad credential.
		return nil, err
	}

	// 2. The startup `user` must be the token's owner. Identity comes from
	// the TOKEN; this parameter is a cross-check, never an override — a
	// client that names someone else is either confused or probing.
	owner, err := e.store.Users.OnCtx(ctx).With(meta.UserID, pat.UserID).Get()
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(owner.Name, startupUser) {
		return nil, deny(DenyUserMismatch)
	}

	// 3a. THE CREDENTIAL CLASS MUST MATCH THE LISTENER.
	//
	// Checked BEFORE any IP work, and that placement is load-bearing:
	// auth.PATAllowsIP returns TRUE for an empty list by contract, because
	// empty means "inherit the admission set" and that is correct under TLS.
	// Teaching it about cleartext would silently change TLS mode too, so this
	// is a separate check with its own reason rather than a condition bolted
	// into a function whose contract other callers depend on.
	//
	// Both directions are refusals, and the token is VALID in each — what
	// refuses it is the listener's mode. That is why neither charges the
	// per-address throttle (matrix §5): a developer whose daemon restarted into the
	// other mode is the common case, not an attacker.
	debugToken := pat.DebugCleartext != 0
	switch {
	case req.Cleartext && !debugToken:
		// Turning cleartext ON refuses every ordinary token in the install.
		// That is the point: a normal credential must never cross a wire that
		// does not protect it.
		return nil, deny(DenyPATNotCleartextDebug)
	case !req.Cleartext && debugToken:
		// A debug token's allowed_ips was never checked against its owner's
		// perimeter, so honouring it here would import that relaxation into
		// production. This refusal is what confines the widening to the mode
		// it was minted for.
		return nil, deny(DenyPATCleartextDebugInTLS)
	}

	// 3b. IP admission.
	//
	// Under TLS: (global ∨ the user's rows), then the token's own narrowing if
	// it sets one.
	//
	// In CLEARTEXT: the token's own list is the ENTIRE gate and the inherited
	// set is NOT consulted. The list is
	// guaranteed non-empty by the mint gate; it is re-checked here anyway,
	// because a store edited by hand must not yield a token admitted from
	// anywhere.
	src := auth.AdmittedByTokenList
	if !req.Cleartext {
		var aerr error
		src, aerr = e.auth.IPAllowedForUser(ctx, nil, pat.UserID, ip)
		if aerr != nil {
			return nil, aerr
		}
		if src == auth.NotAdmitted {
			return nil, deny(DenyIPNotAdmitted)
		}
	} else if strings.TrimSpace(pat.AllowedIPs) == "" {
		return nil, deny(DenyPATIPNarrowed)
	}
	if !auth.PATAllowsIP(pat.AllowedIPs, ip) {
		return nil, deny(DenyPATIPNarrowed)
	}

	// 4. THE TARGET COMES FROM THE CREDENTIAL, NOT FROM THE CLIENT.
	//
	// This is the change that dissolves the ambiguity rather than managing it.
	// Two connections targeting a database called `test` — a local one and a
	// PRODUCTION one — are told apart by WHICH TOKEN was presented, not by a
	// string the client chose, so there is no spelling a client can send that
	// reaches a connection its token does not name.
	//
	// The tombstone is refused here rather than in VerifyPAT: the denial
	// vocabulary lives in this package, and VerifyPAT deliberately collapses
	// every credential failure into one error. It is also AFTER the
	// constant-time compare, so it cannot become a timing oracle for which
	// selectors exist.
	if pat.ConnID == 0 {
		return nil, deny(DenyPATUnscoped)
	}
	connRow, err := e.authorizedConnection(ctx, pat, database)
	if err != nil {
		return nil, err
	}
	a := &WireAuthorization{pat: pat, owner: owner, connRow: connRow, src: src, req: req}
	a.admitter = func(ctx context.Context) (WireSessionResult, error) { return e.admitWire(ctx, a) }
	return a, nil
}

// authorizedConnection reads the token's connection and checks what a login
// needs of it: it exists, the owner holds a grant on it, it is exposed to the
// front door, and the client's database name agrees. Authorization runs it once;
// finalization runs it again, fresh, after the wait.
func (e *Engine) authorizedConnection(ctx context.Context, pat *meta.PAT, database string) (*meta.Connection, error) {
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, pat.ConnID).Get()
	if err != nil {
		if errors.Is(err, dao.ErrNoRows) {
			// A live token naming a row that is gone. There is no database-level
			// foreign key (it cannot be added to a populated
			// table), so this is the guard that makes a dangling reference
			// LOUD rather than silent, and it is why this reason survived the
			// binding change instead of being collapsed into the mismatch.
			return nil, deny(DenyNoSuchDatabase)
		}
		return nil, err
	}
	// A grant on that connection. Read is the floor for holding a session;
	// what the session may then RUN is decided per statement.
	if aerr := e.auth.AuthorizeUser(ctx, pat.UserID, connRow.ID, auth.ActionRead); aerr != nil {
		if errors.Is(aerr, auth.ErrDenied) {
			return nil, deny(DenyNoGrant)
		}
		return nil, aerr
	}
	// And the connection must admit front-door use at all. Opt-in per
	// connection: no target is reachable through this surface
	// by default, so adding the listener does not silently expose every
	// database the daemon knows about.
	if !connectionFrontDoorExposed(connRow) {
		return nil, deny(DenyProfileRefuses)
	}
	// 4c. The `database` field is now a CONSISTENCY CHECK, not a lookup key
	// (as ruled).
	//
	// Checked AFTER the grant so an ungranted caller never produces a
	// mismatch row: a reader of the audit trail would take that row as
	// evidence the connection exists.
	if !wireDatabaseAgrees(database, connRow) {
		return nil, deny(DenyDatabaseMismatch)
	}

	return connRow, nil
}

// ErrClientGone is the cause a front-door admission is cancelled with when its
// client closes, resets, or sends input before it was told it may: nobody is
// listening for an answer, so none is written.
var ErrClientGone = errors.New("exec: the client left while its login was being admitted")

// ErrAdmitFinalizeTimeout means a granted login could not finish opening within
// timeouts.AdmitFinalize: a stalled meta store or target. The front door answers
// it as startup-unavailable (57P03), never as a capacity refusal.
var ErrAdmitFinalizeTimeout = errors.New("exec: the login could not finish opening in time")

// reasonAdmitAborted closes a granted session whose finalization did not hand it
// to the front door.
const reasonAdmitAborted = "admit-aborted"

// Admission steps reported to hookAdmitStep, in the order they are reached.
const (
	admitStepGranted   = "admit.granted"     // the lease is granted; finalization not begun
	admitStepGate      = "finalize.gate"     // the exposure gate is held
	admitStepStanding  = "finalize.standing" // about to re-check the login's standing
	admitStepPinned    = "finalize.pinned"   // the backend is pinned
	admitStepNote      = "finalize.note"     // about to note the PAT's use
	admitStepFinalized = "admit.finalized"   // finalization returned; not yet arbitrated
)

// admitWire waits in the pool's line for an authorized login, then finalizes it:
// the fresh checks, the pin, the startup settings and publication.
//
// ctx is the front door's ADMISSION context. It has no deadline (the line's
// bound is QueueWait, applied by answerBy), and the front door cancels it with
// cause ErrClientGone when the client leaves.
func (e *Engine) admitWire(ctx context.Context, a *WireAuthorization) (WireSessionResult, error) {
	id, err := newSessionID()
	if err != nil {
		return WireSessionResult{}, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		// The authority is the TOKEN, named by its row id. A zero sentinel
		// here is what made the janitor read every wire session as revoked.
		id: id, userID: a.pat.UserID, authority: auth.PATAuthority(a.pat.ID), connID: a.connRow.ID,
		ctx: sctx, cancel: cancel, lastUsed: e.now(),
		// The holder's identity for the idle-in-transaction heartbeat, taken
		// here because here is where it is known. owner.Name is the CANONICAL
		// name, not the client's spelling of it, and pat.ID is the token's row
		// -- never the token.
		holderUser: a.owner.Name, holderIP: a.req.IP, patID: a.pat.ID, acquiredAt: e.now(),
	}
	if h := e.hookBeforeWireAdmit; h != nil {
		h()
	}
	// WAITS ITS TURN RATHER THAN BEING REFUSED, when waiting could cure it, and
	// OUTSIDE EVERY ENGINE LOCK: the wait can last QueueWait. The lease is
	// counted under the connection's pool, so the pools must know it before the
	// first admission, loaded or not.
	e.pools.ensurePoolOf(a.connRow)
	if rerr := e.sessions.admitWithLeaseOrWait(ctx, s, a.connRow.ID, WireSessionOverhead); rerr != nil {
		cancel()
		// AUTHORIZED HERE, WHERE IT WAS ESTABLISHED: the caller may be told the
		// system is full rather than handed the uniform denial that reads as
		// "your credential is wrong".
		if reason, detail, ok := admissionAnswer(rerr); ok {
			return WireSessionResult{}, denyAfterAuthorizationWithDetail(reason, detail)
		}
		return WireSessionResult{}, rerr
	}
	s.mu.Lock()
	s.appName, s.wire = a.req.ApplicationName, true
	s.mu.Unlock()
	e.admitStep(ctx, admitStepGranted)

	fctx, cancelF := context.WithTimeout(ctx, e.admitFinalize)
	defer cancelF()
	// THE GRANT IS OWNED BY THIS GUARD until the result is handed back. It runs on
	// an error and on a panic alike (a panic then continues to the front door's
	// recover), and after runFinalize's own deferred endRun and finish, so
	// closeSession's quiesce never joins a run this goroutine still holds.
	// beginClose is the one atomic claim: if a closer already owns s this loses
	// quietly; otherwise finishClosing releases s.pc through the backend gate and
	// only then the reservation.
	handed := false
	defer func() {
		if !handed {
			e.closeSession(context.WithoutCancel(ctx), s, a.req.IP, reasonAdmitAborted)
		}
	}()
	res, ferr := e.runFinalize(fctx, s, a)
	if h := e.hookAdmitStep; h != nil {
		h(fctx, admitStepFinalized)
	}
	// THE SUCCESS COMMIT POINT. Every result, a nil error included, is judged from
	// the sources that carry the reason. The run context's own stop arrives
	// through context.AfterFunc on another goroutine, so it can lag fctx, and an
	// engine closer can claim s after finalizeWire's last check.
	if err := e.finalizeOutcome(ctx, fctx, s, ferr); err != nil {
		return WireSessionResult{}, err
	}
	handed = true
	return res, nil
}

// runFinalize runs finalizeWire as s's in-flight run, exactly as a statement is
// run: begin marks it busy and runContext registers runCancel, so a closer that
// claims s meanwhile (an exposure sweep, a shutdown) quiesces it -- cancelInFlight,
// then joinInFlight -- before it detaches s.pc. endRun and finish are DEFERRED, so
// a panic cannot leave the slot busy.
func (e *Engine) runFinalize(fctx context.Context, s *session, a *WireAuthorization) (WireSessionResult, error) {
	release, _, err := e.claimSession(fctx, s) // no demotion close reaches a login
	if err != nil {
		return WireSessionResult{}, err
	}
	// THE SESSION'S SILENCE STARTS WHEN THIS RUN ENDS: the release's finish()
	// stamps lastUsed. It was stamped when the session was built, before the
	// wait in line and the pin, so a login that waited would otherwise open
	// already past its ReclaimGrace.
	defer release()
	rctx, endRun := s.runContext(fctx) // a stop, not a cause: finalizeOutcome decides why
	defer endRun()
	return e.finalizeWire(rctx, s, a)
}

// finalizeWire re-checks a granted login and opens it, holding the exposure gate
// for reading -- the gate a disabling writer takes. ctx is the RUN context: it
// says "stop", not why; every early return just stops, and finalizeOutcome
// decides what the client is told.
func (e *Engine) finalizeWire(ctx context.Context, s *session, a *WireAuthorization) (WireSessionResult, error) {
	if err := e.exposure.RLock(ctx); err != nil {
		return WireSessionResult{}, err
	}
	defer e.exposure.RUnlock()
	e.admitStep(ctx, admitStepGate)
	// A DISABLE THAT RAN IN THE GAP between the grant and the gate committed, then
	// closed every registered wire session on the connection, this one included.
	// The close path owns it now: touch nothing.
	if s.get() != sessOpen {
		return WireSessionResult{}, errClosedBeneath
	}
	pat, ip := a.pat, a.req.IP
	connRow, err := e.authorizedConnection(ctx, pat, a.req.Database)
	if err != nil {
		return WireSessionResult{}, err
	}
	e.admitStep(ctx, admitStepStanding)
	v, err := e.resolveStanding(ctx, auth.PATAuthority(pat.ID), pat.UserID, connRow.ID)
	switch {
	case err != nil && ctx.Err() != nil:
		return WireSessionResult{}, err // a stopped run is not a pass
	case err != nil:
		// A store failure is not a revocation (revokeExpiredAuthority's rule):
		// the login opens on the verdict it was authorized with.
		e.logf("session %s: re-checking standing failed, opening on the authorization's verdict: %v", s.id, err)
	case !v.Standing:
		return WireSessionResult{}, deny(DenyNoGrant)
	}

	var statuses map[string]string
	if connRow.Engine.SpeaksPostgresWire() {
		pinCtx, cancelPin := context.WithTimeout(ctx, pinAllowance)
		defer cancelPin()
		// Pin now. A pin that waited out its share is a capacity refusal, framed
		// like admission's; anything else (a slow or unreachable target) is
		// returned for the front door's startup classification. On any refusal
		// below, admitWire's guard closes the session and releases what is pinned.
		pc, perr := e.pinWireSession(pinCtx, s, connRow)
		if perr != nil {
			if reason, detail, ok := admissionAnswer(perr); ok {
				return WireSessionResult{}, denyAfterAuthorizationWithDetail(reason, detail)
			}
			return WireSessionResult{}, perr
		}
		e.admitStep(ctx, admitStepPinned)
		// Row 3.1 fails CLOSED: no reporter, no statuses, or either encoding
		// key missing is a refusal, not an acceptance — the lease's encoding
		// must be ESTABLISHED as UTF8, not merely not-known-to-be-otherwise.
		if r, ok := e.reporterFor(pc).(golibpg.ParameterStatusReporter); ok {
			statuses = r.ReportedParameterStatuses()
		}
		if enc, ok := leaseEncodingRefusal(statuses); ok {
			// Row 3.1: the lease is pinned UTF8; autodb does not transcode.
			e.auditBounded(context.WithoutCancel(ctx), pat.UserID, ip, "wire_lease_encoding_refused",
				fmt.Sprintf("conn %d: session %s: target reports %s", connRow.ID, s.id, enc))
			// Nothing has run on this backend — the refusal is about what the
			// target reported at startup, not about anything this session did
			// — so it is given back untouched, before the guard's close. A pool
			// backend is then closed by pgxpool's release hook (reusesBackend).
			pc.Discard()
			s.mu.Lock()
			s.pc = nil
			s.mu.Unlock()
			return WireSessionResult{}, deny(DenyLeaseEncoding)
		}
		// Startup GUCs: judged by the SAME denylist a SET from this session
		// meets, then applied to the pinned backend so the reported
		// ParameterStatus set the client receives already reflects them. A
		// refusal partway through leaves settings ON the backend, so it is not
		// handed back here: the guard's close sends it through the release gate
		// like any other used backend.
		if len(a.req.StartupGUCs) > 0 {
			if gerr := e.applyStartupGUCs(pinCtx, s, pc, a.req.StartupGUCs, pat.UserID, ip, connRow.ID); gerr != nil {
				return WireSessionResult{}, deny(DenyStartupGUC)
			}
			if r, ok := e.reporterFor(pc).(golibpg.ParameterStatusReporter); ok {
				statuses = r.ReportedParameterStatuses() // live: reflects the SETs
			}
		}
	} else if len(a.req.StartupGUCs) > 0 {
		// A non-PostgreSQL target has no session to apply a GUC to. Refuse
		// rather than pretend: an accepted setting that silently did nothing
		// is the behaviour a client cannot detect.
		e.auditBounded(context.WithoutCancel(ctx), pat.UserID, ip, "wire_startup_guc_refused",
			fmt.Sprintf("conn %d: session %s: target engine %q has no session settings", connRow.ID, s.id, connRow.Engine))
		return WireSessionResult{}, deny(DenyStartupGUC)
	}
	e.admitStep(ctx, admitStepNote)
	// Best effort: a failed note is logged by NotePATUse and the login opens.
	e.auth.NotePATUse(ctx, pat)
	// THE STOP IS AUTHORITATIVE AT THE COMMIT POINT. NotePATUse swallows its own
	// errors, and the steps above answer with their own denials; once ctx has
	// stopped, none of that is a success.
	if err := ctx.Err(); err != nil {
		return WireSessionResult{}, err
	}
	return WireSessionResult{
		SessionID: s.id, UserID: pat.UserID, ConnID: connRow.ID,
		AdmissionSource: a.src, PATName: pat.Name, UserName: a.owner.Name,
		ParameterStatuses: statuses, ApplicationName: a.req.ApplicationName,
	}, nil
}

// errClosedBeneath stops a finalization whose session an engine closer has
// claimed; finalizeOutcome answers with that closer's reason.
var errClosedBeneath = errors.New("exec: the session was closed while its login was being finalized")

// finalizeOutcome judges a finalization result, success or failure, from the
// three sources that carry the reason, in this order. The run context is not one
// of them. It returns nil only for a success none of them overrides.
//
//   - a departed client outranks everything: there is no one to answer;
//   - an engine closer outranks the budget: it decided first and owns the teardown;
//   - the budget outranks a step's own denial: a step that ran past it was cut off.
func (e *Engine) finalizeOutcome(admitCtx, fctx context.Context, s *session, err error) error {
	switch {
	case errors.Is(context.Cause(admitCtx), ErrClientGone):
		return ErrClientGone
	case s.get() != sessOpen:
		return closedBeneath(s)
	case errors.Is(fctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrAdmitFinalizeTimeout, fctx.Err())
	default:
		return err
	}
}

// closedBeneath is the answer for a login whose session an engine closer claimed
// before it was handed over: a disabled exposure is the connection refusing the
// front door; anything else is the engine going away.
func closedBeneath(s *session) error {
	s.mu.Lock()
	why := s.closeWhy
	s.mu.Unlock()
	if why == reasonExposureDisabled {
		return deny(DenyProfileRefuses)
	}
	if reason, detail, ok := admissionAnswer(ErrEngineClosing); ok {
		return denyAfterAuthorizationWithDetail(reason, detail)
	}
	return ErrEngineClosing
}

// resolveStanding is auth.ResolveStanding, or the test hook standing in for it.
func (e *Engine) resolveStanding(ctx context.Context, ref auth.AuthorityRef, userID, connID int64) (auth.StandingVerdict, error) {
	if h := e.hookStanding; h != nil {
		return h()
	}
	return e.auth.ResolveStanding(ctx, ref, userID, connID)
}

// admitStep reports an admission step to the test hook, if one is installed.
func (e *Engine) admitStep(ctx context.Context, step string) {
	if h := e.hookAdmitStep; h != nil {
		h(ctx, step)
	}
}

// connectionFrontDoorExposed reads the connection's explicit exposure decision.
// Capability profiles do not participate: reachability is decided before any
// statement exists and must default closed independently of SQL policy.
func connectionFrontDoorExposed(row *meta.Connection) bool {
	return row != nil && row.FrontDoorExposed != 0
}

// leaseEncodingRefusal reports why the lease cannot be established as UTF8:
// no reported set, a missing server_encoding or client_encoding, or a value
// that is not UTF8. SQL_ASCII is refused too: it means the server validates
// nothing, which is not "UTF8-compatible" for a relay that does not transcode.
// Absence is a refusal — the rule fails closed.
func leaseEncodingRefusal(statuses map[string]string) (string, bool) {
	if statuses == nil {
		return "no reported statuses", true
	}
	for _, k := range []string{"server_encoding", "client_encoding"} {
		v, ok := statuses[k]
		if !ok {
			return k + " missing", true
		}
		switch strings.ToUpper(strings.ReplaceAll(v, "-", "")) {
		case "UTF8":
		default:
			return k + "=" + v, true
		}
	}
	return "", false
}

// reporterFor returns the value whose ParameterStatusReporter capability is
// consulted at open: the pinned connection itself, or, in tests, whatever
// hookWrapPinned substitutes (a wrapper WITHOUT the capability, or one that
// reports an incomplete set) so the fail-closed arms can be observed.
func (e *Engine) reporterFor(pc golibpg.PinnedConn) any {
	if e.hookWrapPinned != nil {
		return e.hookWrapPinned(pc)
	}
	return pc
}

// wireDatabaseAgrees reports whether the client's startup `database` names the
// connection its token is bound to.
//
// It REPLACED a lookup. The field used to select which connection to open;
// now the token has already decided that, and this only asks whether the
// client agrees. The distinction matters because it changes what a
// disagreement means: not "no such database" but "you asked for something
// other than what this credential reaches", which is refused rather than
// silently substituted.
//
// Three accepted spellings:
//
//   - the connection's NAME — what works today;
//   - its TARGET DATABASE NAME, when one is known — what every introspecting
//     client sends after discovering the target's real databases, and the
//     whole reason this ADR exists. Skipped when empty, which is the ordinary
//     case for an engine with no derivable name (r7);
//   - `conn:<id>` — the explicit disambiguator, which survives a rename.
//
// TrimSpace and ParseInt are KEPT deliberately. Both are today's behaviour —
// a trailing space in a client's config field is a typo rather than an
// intent, and `conn:01` names the same row as `conn:1` — so tightening them
// here would be a silent change to who can connect, smuggled in under a
// change about something else.
//
// The name comparisons are EXACT BYTES, and that is not the same rule the
// startup `user` gets a few lines above (EqualFold). The asymmetry is each
// namespace's own: two connections whose names differ only in case COEXIST
// (`connections.name` is UNIQUE but case-sensitive on both engines), and
// PostgreSQL database names are case-sensitive too, so folding either would
// accept a client that asked for a DIFFERENT REAL THING. `user` can fold
// because identity comes from the token and no other principal the client
// could have meant would then be served.
func wireDatabaseAgrees(database string, connRow *meta.Connection) bool {
	database = strings.TrimSpace(database)
	if rest, ok := strings.CutPrefix(database, "conn:"); ok {
		id, perr := strconv.ParseInt(rest, 10, 64)
		return perr == nil && id == connRow.ID
	}
	if database == connRow.Name {
		return true
	}
	return connRow.TargetDB != "" && database == connRow.TargetDB
}

// CloseWireSession releases a front-door session and everything it reserved.
func (e *Engine) CloseWireSession(ctx context.Context, id SessionID, userID int64, ip, reason string) {
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return
	}
	e.closeSession(ctx, s, ip, reason)
}

// applyStartupGUCs admits and applies the client's startup settings on the
// pinned backend, in sorted order so refusals are deterministic. The admission
// is admitWireSet with Local=false — the same function a SET statement meets —
// and the application is a real `SET name TO 'value'` on the session's own
// backend (PostgreSQL applies startup parameters as session settings; so do
// we, on the session that is now ours). The target's refusal (an unknown
// setting, a bad value — the cases PostgreSQL would FATAL at startup) is a
// refusal here too, audited with the target's own message.
func (e *Engine) applyStartupGUCs(ctx context.Context, s *session, pc golibpg.PinnedConn, gucs map[string]string, userID int64, ip string, connID int64) error {
	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return perr
	}
	sq, ferr := rawFace(pc)
	if ferr != nil {
		return ferr
	}
	names := make([]string, 0, len(gucs))
	for k := range gucs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, raw := range names {
		name := strings.ToLower(raw)
		sqlText := "SET " + quoteIdent(name) + " TO " + quoteLiteral(gucs[raw])
		stmt := Statement{Verb: "SET", Class: ClassControl}
		admitErr, opErr := e.runSessionStateAdmission(pol, admission.PhysWire, false, stmt, sqlText)
		if opErr != nil {
			return opErr
		}
		if admitErr != nil {
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: %v", connID, s.id, name, admitErr))
			return admitErr
		}
		var targetErr *pgconn.PgError
		_, derr := e.sessionSimpleQuery(ctx, sq, sessionSQLAutodb, sqlText, func(m golibpg.ExtendedMessage) error {
			if m.Kind == "ErrorResponse" && targetErr == nil {
				targetErr = m.Err
			}
			return nil
		})
		switch {
		case derr != nil:
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: wire: %v", connID, s.id, name, derr))
			return derr
		case targetErr != nil:
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: target: %s", connID, s.id, name, targetErr.Message))
			return targetErr
		}
	}
	e.auditBounded(ctx, userID, ip, "wire_startup_gucs_applied",
		fmt.Sprintf("conn %d: session %s: %s", connID, s.id, strings.Join(names, ",")))
	return nil
}

// quoteIdent renders a setting name as a double-quoted identifier.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// quoteLiteral renders a value as a single-quoted string literal. PostgreSQL
// accepts the quoted form for every GUC type (numeric and boolean included).
func quoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
