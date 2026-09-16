package frontdoor

import (
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The front door's own outcomes, declared per PHASE.
//
// The phases are the ones that exist, not the ones a tidy diagram would have.
// Credential verification and session open are a single atomic call in the
// engine today, so they are one producer there and appear here only as the
// identities this package renders on their behalf.
const (
	// ProducerAccept is the synchronous outer loop: reserve or refuse, before
	// a goroutine, a reader or a TLS buffer exists.
	ProducerAccept = outcome.ProducerID("accept")
	// ProducerStartup is TLS negotiation and the startup packet.
	ProducerStartup = outcome.ProducerID("startup")
	// ProducerCancel is the cancel-request branch: terminal, and it never
	// authenticates.
	ProducerCancel = outcome.ProducerID("cancel")
	// ProducerAuthOpen is the credential exchange this package drives around
	// the engine's single open call.
	ProducerAuthOpen = outcome.ProducerID("authenticate-and-open")
	// ProducerHandshake is the success sequence and the transition into the
	// session.
	ProducerHandshake = outcome.ProducerID("handshake")
	// ProducerServe is the session loop and its owned teardown.
	ProducerServe = outcome.ProducerID("serve")
	// ProducerRequestAcquire is one request's attempt to obtain a backend.
	//
	// SEPARATE FROM SERVE, because what it produces is not an ending. Serve's
	// outcomes are what finishes a CONNECTION -- the peer closed, the session
	// errored -- and a backend that could not be opened finishes neither: the
	// request fails, the client is told so, and the same session carries on
	// and may be served by a different backend on its next statement. Filing a
	// surviving request's outcome in the terminal connection phase's manifest
	// makes "what can end this connection" unanswerable by listing something
	// that does not.
	//
	// It is not a lifecycle phase either, and must not be made one. A phase
	// runs at most once per connection in a fixed order; acquisition runs once
	// per request, any number of times, on a connection that is already past
	// every phase. A producer is the right unit for it because membership is
	// the producer's and nothing about a producer claims to be a phase.
	ProducerRequestAcquire = outcome.ProducerID("request-acquisition")
	// ProducerLifecycle owns faults in the RUNNER itself -- a phase that could
	// not be looked up, an identity nobody declared.
	//
	// SEPARATE FROM EVERY PHASE, because it is not a thing that happened to
	// the connection: it is a thing wrong with us. Attributing it to the phase
	// it interrupted would file our own bug under that phase's vocabulary, and
	// an operator counting startup failures would count our defects among them.
	ProducerLifecycle = outcome.ProducerID("lifecycle-infrastructure")
)

// The endings that are not refusals.
//
// THEY ARE THE CLOSE REASONS THE TRAIL ALREADY RECORDS, deliberately: naming
// them anything else would change what an operator greps for in order to make
// a registry tidier. What changes is that they are now declared, so a phase
// cannot end on one nobody classified -- which is the whole reason this is an
// outcome registry rather than a refusal registry.
const (
	OutcomeStartupFailed = "startup-failed"
	// OutcomeAuthReadFailed is the peer abandoning the credential exchange, or
	// sending something that is not a password where one belongs. Theirs, and
	// charged: it is the same act as grinding a credential, one step earlier.
	OutcomeAuthReadFailed = "auth-read-failed"
	// OutcomeAuthWorkerBusy and OutcomeAuthSetupFailed are OURS.
	//
	// They used to share the identity above, which made the registry say one
	// thing -- never charged -- while the code charged whichever of the three
	// had happened to set a separate Boolean. A peer who waited for a worker
	// we could not spare presented something we never looked at.
	OutcomeAuthWorkerBusy  = "auth-worker-unavailable"
	OutcomeAuthSetupFailed = "auth-setup-failed"
	OutcomeHandshakeWrite  = "handshake-write-failed"
	OutcomeDeadlineArm     = "deadline"
	OutcomeSessionError    = "session-error"
	OutcomePeerClosed      = "peer-closed"
	// OutcomeDialFailed is a request whose backend connection could not be
	// opened, at any stage: the name, the socket, the TLS handshake, the
	// startup exchange, the credential autodb presents upstream, or the
	// settings re-applied to a fresh backend.
	//
	// ONE IDENTITY FOR EVERY STAGE, on purpose. The stage is a field on the
	// audit detail rather than a second identity, because an identity per
	// stage is a vocabulary the client can count: a caller who could tell
	// "DNS" from "TLS" from "upstream authentication" apart by the shape of
	// what came back would be reading our topology off our error surface.
	//
	// THIS STRING IS ALSO THE STABLE RULE ID THE WIRE CARRIES IN DETAIL, and
	// denial.go's DialFailedRule is defined FROM it rather than beside it.
	// They were two literals that differed -- the declaration said
	// "dial-failed" and every raise site wrote "frontdoor/dial-failed" -- so
	// the declared identity was one nothing could reach and the recorded one
	// was one nothing had declared, and neither half could notice because
	// neither half ever met the other. One constant is what makes an operator
	// reading a client's complaint and an operator grepping the trail land on
	// the same row.
	OutcomeDialFailed = "frontdoor/dial-failed"
	// EventDialFailed is the audit kind for one.
	//
	// NOT fd.refused. That kind means we considered a caller's work and
	// declined it; a target that could not be reached judged nothing, and
	// filing it under refusals puts a target outage among the numbers an
	// operator watches for policy and for credential attacks.
	EventDialFailed = "fd.dial_failed"
	// OutcomeConnectionUnusable is a request whose connection cannot serve it
	// as configured: an engine this build does not implement, a stored DSN the
	// engine's own parser rejects, a driver pool that would not construct, or a
	// resolved driver missing a capability this product's guarantees depend on.
	//
	// SEPARATE FROM OutcomeDialFailed BECAUSE THE TWO ARE ANSWERED BY DIFFERENT
	// PEOPLE. A dial failure sends an operator to a network; this sends them to
	// a connection row or a dependency. Folding them together also falsifies
	// the dial failure's own attempt count, which exists so that a target that
	// failed once can be told from one being retried in a loop -- a
	// configuration failure takes no permit and opens no socket, so its honest
	// count is zero and it must not be reported as one.
	//
	// THIS STRING IS ALSO THE STABLE RULE ID THE WIRE CARRIES IN DETAIL, and
	// denial.go's ConnectionUnusableRule is defined FROM it, for the reason the
	// dial-failed pair is: an identity spelled twice is an identity that can be
	// spelled two ways, and neither half can notice.
	OutcomeConnectionUnusable = "frontdoor/connection-unusable"
	// EventConnectionUnusable is the audit kind for one.
	//
	// NOT fd.refused and NOT fd.dial_failed. Nothing about the caller's work
	// was judged, so it is not a refusal; nothing was dialled, so filing it
	// among target outages would put an install's own misconfiguration into
	// the numbers an operator watches for target health.
	EventConnectionUnusable = "fd.connection_unusable"
	// OutcomeStartupConnectionUnavailable is a wire session that could not
	// START because the secret store would not answer.
	//
	// NAMED FOR WHAT THE CALLER IS ENTITLED TO KNOW, not for what happened.
	// The identity travels in the frame's DETAIL as the stable rule id, so an
	// identity spelling "store-locked" would tell a client holding only a
	// socket that our secret store is locked -- which is a fact about the
	// estate, arrived at through the one field everyone assumed was safe
	// because it is "just an id". The operator gets the real cause in the
	// audit, where it belongs.
	//
	// SEPARATE FROM THE REQUEST-TIME IDENTITIES BECAUSE THE LIFECYCLE IS
	// DIFFERENT AND THE CLIENT CONTRACT IS DIFFERENT. There is no session yet:
	// no ReadyForQuery has been sent, nothing survives, and the frame is FATAL.
	// Registering one identity for both lifecycles would put "the session
	// survived" and "the connection ended" under one row, and an operator
	// could not tell which happened.
	OutcomeStartupConnectionUnavailable = "frontdoor/startup-connection-unavailable"
	// OutcomeStartupConnectionUnusable is a wire session that could not start
	// because the connection cannot serve requests as configured.
	OutcomeStartupConnectionUnusable = "frontdoor/startup-connection-unusable"
	// EventAuthOperational is the audit kind for an error-driven ending in the
	// credential exchange.
	//
	// NOT fd.auth_denied. A peer may have presented a perfectly good
	// credential and been refused because our own store was unreachable;
	// filing that under the denial kind inflates the number an operator
	// watches for credential attacks with events that are our fault. The code
	// already said so in a comment and then did the opposite, because the
	// store outage travelled down the common denial path.
	EventAuthOperational = "fd.auth_failed"

	// OutcomeInternalError is a fault in OUR code -- a phase that could not
	// run, an identity nobody declared. Never the peer's doing and never
	// charged to them.
	OutcomeInternalError = "internal-error"
)

// EventLifecycleFault is the audit kind for a fault in the runner itself.
//
// NEVER fd.budget_refuse. That kind means a peer met a limit, and filing our
// own defects under it puts them among the capacity numbers an operator sizes
// the estate from.
const EventLifecycleFault = "fd.lifecycle_fault"

// The cancel branch's identities.
//
// THESE WERE HOMELESS, and that is the whole reason this registry is named for
// outcomes rather than refusals. A cancel request is not a denial: nothing was
// refused, a request was handled and the connection closed. So these three
// belonged to no vocabulary at all and lived as bare string literals at the
// call site, where nothing could enumerate them, classify them, or notice one
// going missing.
//
// They are constants now so the audit trail and the registry cannot disagree
// about what an occurrence is called. The values are unchanged.
const (
	EventCancelReceived = "fd.cancel_received"
	EventCancelApplied  = "fd.cancel_applied"
	EventCancelStale    = "fd.cancel_stale"
)

// The startup phase's failure identities that are not denial reasons.
//
// HOMELESS IN THE SAME WAY THE CANCEL IDENTITIES WERE. A TLS handshake that
// fails, a startup code nobody recognises, a frame that will not read, and a
// peer that hung up before asking for anything are all outcomes of the startup
// phase -- and none of them is a denial, so none had a place in a vocabulary
// named for denials. They lived as bare literals at the call site.
//
// Their charges are the ones the code already applies, read off it rather than
// decided here: the first three are attributable to the peer and counted, and
// the last is explicitly not, because throttling a health probe for being a
// health probe is an outage we would have configured ourselves.
const (
	OutcomeTLSHandshake    = "tls-handshake"
	OutcomeStartupCode     = "startup-code-unknown"
	OutcomeStartupUnread   = "startup-unreadable"
	OutcomePeerGoneAtStart = "peer-gone-before-startup"
)

// Outcomes declares what each phase of this package can emit.
//
// CHARGES ARE THE RULED ONES AND NOTHING HERE RECLASSIFIES ANYTHING. Accept
// refusals are capacity or throttle decisions and are not charged; startup and
// pre-auth protocol failures are the peer speaking the protocol wrongly and
// are; our own store failures are Operational and never the peer's doing.
//
// The one that is easy to get wrong, and was got wrong, is the accept-time
// group. A connection refused because one host is already using its whole
// allowance has not failed a credential -- it has met a concurrency ceiling --
// and charging it is how a per-source limit becomes a per-source ban.
func Outcomes() []outcome.Registration {
	refusal := func(r denialReason, c outcome.Charge) outcome.Decl {
		return outcome.Decl{ID: outcome.ReasonID(r), Kind: outcome.Refusal, Charge: c}
	}
	control := func(id string) outcome.Decl {
		return outcome.Decl{ID: outcome.ReasonID(id), Kind: outcome.Control, Charge: outcome.None}
	}
	operational := func(r denialReason) outcome.Decl {
		return outcome.Decl{ID: outcome.ReasonID(r), Kind: outcome.Operational, Charge: outcome.None}
	}

	return []outcome.Registration{
		{Producer: ProducerAccept, Outcomes: []outcome.Decl{
			// Charged: this one IS a per-source failure budget being spent.
			refusal(reasonSourceThrottled, outcome.Credential),
			// Not charged: capacity and concurrency. A peer refused because
			// the system is full has done nothing wrong, and charging it is
			// what turned a busy morning into a banned address.
			refusal(reasonConnectionCap, outcome.Capacity),
			refusal(reasonSourceConnCap, outcome.Capacity),
			refusal(reasonPreAuthConnCap, outcome.Capacity),
			refusal(reasonControlLaneExhausted, outcome.Capacity),
		}},
		{Producer: ProducerStartup, Outcomes: []outcome.Decl{
			refusal(reasonPlaintextStartup, outcome.Protocol),
			refusal(reasonDirectTLS, outcome.Protocol),
			refusal(reasonUnsupportedMajor, outcome.Protocol),
			refusal(reasonStartupMalformed, outcome.Protocol),
			refusal(reasonStartupParamRefus, outcome.Protocol),
			refusal(reasonStartupGUCCount, outcome.Protocol),
			refusal(reasonStartupOptionsMalformed, outcome.Protocol),
			refusal(reasonStartupDuplicateKey, outcome.Protocol),
			refusal(reasonPreAuthOversize, outcome.Protocol),
			// NOTE: reasonStoreLocked is NOT declared here. It has no
			// production raise site -- the renderer still special-cases it, so
			// the behaviour is ready for one -- and a declared identity that
			// nothing can emit is a registry describing a system that does not
			// exist. It is declared when something raises it.
			//
			// The identities above are denial reasons; these four are the
			// phase's other ways to end, and they end it WITHOUT a frame --
			// a peer speaking raw TLS cannot read a PostgreSQL error.
			{ID: OutcomeTLSHandshake, Kind: outcome.Refusal, Charge: outcome.Protocol},
			{ID: OutcomeStartupCode, Kind: outcome.Refusal, Charge: outcome.Protocol},
			{ID: OutcomeStartupUnread, Kind: outcome.Refusal, Charge: outcome.Protocol},
			// NOT CHARGED, and the code already says why: a connection that
			// opened and closed before asking anything is a port scan or a
			// load balancer's health probe, and banning those is an outage of
			// our own making.
			{ID: OutcomePeerGoneAtStart, Kind: outcome.Operational, Charge: outcome.None},
			// A startup that failed for a reason with no taxonomy of its own,
			// and a fault in our own phase wiring.
			{ID: OutcomeStartupFailed, Kind: outcome.Operational, Charge: outcome.None},
		}},
		{Producer: ProducerCancel, Outcomes: []outcome.Decl{
			// A cancel presents no credential, so it cannot fail one. Control
			// rather than Refusal: nothing was denied, a request was handled.
			control(EventCancelReceived),
			control(EventCancelApplied),
			control(EventCancelStale),
		}},
		{Producer: ProducerAuthOpen, Outcomes: credentialOutcomes(refusal, operational)},
		{Producer: ProducerHandshake, Outcomes: []outcome.Decl{
			// A write that failed is OURS or the network's, never a thing the
			// peer did wrong, so none of these is charged.
			{ID: OutcomeHandshakeWrite, Kind: outcome.Operational, Charge: outcome.None},
			{ID: OutcomeDeadlineArm, Kind: outcome.Operational, Charge: outcome.None},
		}},
		{Producer: ProducerLifecycle, Outcomes: []outcome.Decl{
			{ID: OutcomeInternalError, Kind: outcome.Operational, Charge: outcome.None},
		}},
		// The conditions a session's held prepared statements and portals
		// produce, declared from the register that renders them so the two
		// cannot drift.
		//
		// THE RUNTIME REGISTER ONLY. held_objects.go also carries a RESERVED
		// table, whose rows have no producer yet; declaring those here would
		// make this manifest claim a path the code does not have, and this
		// manifest is what answers "what can happen in this phase".
		{Producer: ProducerHeldObjects, Outcomes: heldObjectDecls()},
		{Producer: ProducerServe, Outcomes: []outcome.Decl{
			// The ordinary ending: the client said goodbye, or went away.
			{ID: OutcomePeerClosed, Kind: outcome.Control, Charge: outcome.None},
			{ID: OutcomeSessionError, Kind: outcome.Operational, Charge: outcome.None},
		}},
		// A REQUEST'S BACKEND COULD NOT BE OPENED.
		//
		// Operational, because it is driven by an error rather than by a
		// decision anyone took. NotApplicable rather than None, because this
		// happens on an authenticated session that is already past every
		// accept-time budget: there is no per-source counter in reach, so "we
		// decided not to charge it" would claim a decision nobody had the
		// opportunity to make.
		//
		// REGISTERED ONLY AFTER BOTH CLIENTS PROVED THE SESSION SURVIVES. Real
		// pgx and a real pgjdbc program each hit the shape in the simple AND
		// the extended protocol, kept the connection, and then ran real work on
		// it against a real target. If a client is ever found that treats the
		// chosen code as connection-fatal, the CODE changes and this row stays
		// — the promise is that the session survives, and the SQLSTATE is
		// whatever keeps that promise true in real clients.
		{Producer: ProducerRequestAcquire, Outcomes: []outcome.Decl{
			{ID: OutcomeDialFailed, Kind: outcome.Operational, Charge: outcome.NotApplicable},
			// Operational for the same reason: error-driven, and answerable by
			// nobody holding a socket. NotApplicable rather than None because
			// the caller did nothing this could be charged against -- their
			// credential was accepted and their statement was never judged.
			{ID: OutcomeConnectionUnusable, Kind: outcome.Operational, Charge: outcome.NotApplicable},
		}},
	}
}

// credentialOutcomes is what the credential phase can end on.
//
// IT DECLARES THE ENGINE'S IDENTITIES AS WELL AS ITS OWN, and that is the case
// that forced membership onto the producer rather than onto the reason. The
// engine RAISES a capacity refusal; this phase is where it reaches the wire,
// so this phase can end on it and must say so. An earlier shape put a single
// phase on the reason, which makes one of the two lie about who it belongs to.
//
// The engine's half is DERIVED from the engine's own registration rather than
// restated here. A second list would fall behind the first, and the way it
// would fail is the worst available: a refusal the engine raises and this
// phase has not declared is refused by the runner as a programming error --
// during an incident, on the path that was already refusing somebody.
func credentialOutcomes(
	refusal func(denialReason, outcome.Charge) outcome.Decl,
	operational func(denialReason) outcome.Decl,
) []outcome.Decl {
	out := []outcome.Decl{
		// OURS: the store would not answer, or there is no credential store
		// behind this listener yet. A peer holding a perfectly good token
		// meets these through no fault of their own, so neither is charged.
		//
		// THE STORE OUTAGE IS OPERATIONAL, and it still writes a frame.
		//
		// It was briefly a refusal on the grounds that it renders one, which
		// conflated three separate questions. It is error-driven (kind), ours
		// (charge), and owed the uniform denial (wire) -- and the wire is
		// carried on the outcome, so it need not distort the kind to get one.
		{ID: outcome.ReasonID(reasonAuthStoreError), Kind: outcome.Operational, Charge: outcome.None},
		// OURS TOO, AND ARRIVING HERE FOR A REASON THIS PACKAGE GOT WRONG
		// ONCE. A PostgreSQL-wire session pins its backend inside
		// OpenWireSessionWith, before the client sees ReadyForQuery, so a
		// locked store and a connection this install cannot serve both
		// surface DURING the credential phase. They were being folded into
		// the store-outage identity and answered with the uniform credential
		// denial, which told a developer holding a good token that their
		// CREDENTIAL was wrong -- the exact shape of the lockout this work
		// exists to fix. Neither is charged: the peer did nothing.
		{ID: outcomeID(OutcomeStartupConnectionUnavailable), Kind: outcome.Operational, Charge: outcome.None},
		{ID: outcomeID(OutcomeStartupConnectionUnusable), Kind: outcome.Operational, Charge: outcome.None},
		// A listener with no credential store behind it yet is a DECISION not
		// to serve, taken before anything failed.
		refusal(reasonNoCredentialStore, outcome.None),
		// A credential exchange the peer abandoned mid-way, and a fault in our
		// own phase wiring. Neither is a refusal; both end the connection.
		// CHARGED, because it is the peer's doing. The registry is the only
		// place that decides this now.
		{ID: OutcomeAuthReadFailed, Kind: outcome.Operational, Charge: outcome.Protocol},
		{ID: OutcomeAuthWorkerBusy, Kind: outcome.Operational, Charge: outcome.None},
		{ID: OutcomeAuthSetupFailed, Kind: outcome.Operational, Charge: outcome.None},
		// THEIRS: a frame that is not a password where a password belongs.
		// This is the credential exchange being spoken wrongly, which is the
		// same kind of thing as grinding it.
		refusal(reasonPreAuthProtocolViolation, outcome.Protocol),
	}
	return append(out, exec.Registration().Outcomes...)
}
