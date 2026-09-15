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
	// OutcomeInternalError is a fault in OUR code -- a phase that could not
	// run, an identity nobody declared. Never the peer's doing and never
	// charged to them.
	OutcomeInternalError = "internal-error"
)

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
		{Producer: ProducerServe, Outcomes: []outcome.Decl{
			// The ordinary ending: the client said goodbye, or went away.
			{ID: OutcomePeerClosed, Kind: outcome.Control, Charge: outcome.None},
			{ID: OutcomeSessionError, Kind: outcome.Operational, Charge: outcome.None},
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
		// meets these through no fault of their own.
		operational(reasonAuthStoreError),
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
