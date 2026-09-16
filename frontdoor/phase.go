package frontdoor

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The connection lifecycle as phases, and what a phase is allowed to conclude.
//
// THE PHASES ARE THE ONES THAT EXIST. An earlier design named six, and two of
// them were fiction: credential verification and session open are a single
// atomic call in the engine today -- one call verifies the PAT, takes the
// reservation, pins the backend, applies encoding and settings, and publishes
// the session. Splitting that needs its own token, lock and rollback design,
// which is a behaviour change and not this. Accept was in the wrong place too:
// admission, refusal and ticket release happen in the accept loop, before any
// goroutine exists.
//
// So these wrap what the code does, and the value of that is precisely that a
// reader can check them against it.

// PhaseName identifies a phase for the runner and the record.
type PhaseName string

const (
	// PhaseAccept is the synchronous outer loop's reserve-or-refuse. It
	// returns the linear ownership token; nothing has been allocated for this
	// connection yet beyond the socket itself.
	PhaseAccept PhaseName = "accept"
	// PhaseStartup is TLS negotiation and the startup packet.
	PhaseStartup PhaseName = "startup"
	// PhaseCancel is the cancel-request branch: terminal, and it never
	// authenticates.
	PhaseCancel PhaseName = "cancel"
	// PhaseAuthenticateAndOpen is the engine's single atomic call, wrapped
	// whole.
	PhaseAuthenticateAndOpen PhaseName = "authenticate-and-open"
	// PhaseHandshake is the pre-loop transition into the session.
	PhaseHandshake PhaseName = "handshake"
	// PhaseServe is the session loop and its teardown.
	PhaseServe PhaseName = "serve"
)

// CancelMode records how a phase responds to cancellation TODAY.
//
// RECORDED, NOT IMPOSED, and the distinction is the whole point. The phases
// use a mixture of socket deadlines, context deadlines, and teardown
// deliberately run with cancellation detached. Forcing one model would change
// behaviour under cover of a refactor -- and the one that would break is the
// detached teardown, which is detached on purpose so that a shutdown does not
// abandon the cleanup it triggered.
//
// Changing any of these is later work with its own review.
type CancelMode uint8

const (
	// CancelUnset is the zero value and is a programming error: a phase that
	// has not said what it does with cancellation has not been looked at.
	CancelUnset CancelMode = iota
	// CancelBySocketDeadline: bounded by a deadline on the connection, not by
	// the context.
	CancelBySocketDeadline
	// CancelByContext: the phase passes the context down and it is honoured.
	CancelByContext
	// CancelDetached: run on a context whose cancellation has been dropped,
	// deliberately, so that the work finishes even as the thing that caused
	// it is shutting down.
	CancelDetached
)

func (m CancelMode) String() string {
	switch m {
	case CancelBySocketDeadline:
		return "socket-deadline"
	case CancelByContext:
		return "context"
	case CancelDetached:
		return "detached"
	}
	return "unset"
}

// ReplayMode records whether a phase may be run again.
type ReplayMode uint8

const (
	// ReplayUnset is the zero value and is a programming error.
	ReplayUnset ReplayMode = iota
	// Pure: running it twice has the same effect as running it once.
	Pure
	// RetryBeforeEffect: it may be retried up to the point where it has an
	// effect, and not after.
	RetryBeforeEffect
	// ExactlyOnce: it REFUSES a rerun rather than performing one. Nothing
	// promises a repeat the code cannot deliver.
	ExactlyOnce
)

func (m ReplayMode) String() string {
	switch m {
	case Pure:
		return "pure"
	case RetryBeforeEffect:
		return "retry-before-effect"
	case ExactlyOnce:
		return "exactly-once"
	}
	return "unset"
}

// verdict is what a phase concluded.
//
// UNEXPORTED, and that is the mechanism rather than an accident: with the
// field unexported and the constructors the only way to set it, an outcome
// cannot be assembled from outside this package, and the set of things a phase
// can conclude is the set of constructors below.
type verdict uint8

const (
	// verdictUnset is the zero value, and a runner that meets it stops. A
	// phase that returned the zero Outcome has fallen off the end of its own
	// logic, and the safe reading of "I did not say" is never "carry on".
	verdictUnset verdict = iota
	verdictContinue
	verdictRefuse
	verdictOperational
	verdictTerminalControl
)

// Outcome is what one phase concluded.
//
// THERE IS NO WAIT, and there is no state that could become one.
//
// An earlier design asserted in a test that no phase returns a wait. A test is
// deletable and an assertion is a thing a later author removes while making
// something else pass. This is structural instead: there are exactly four
// constructors, the discriminant is unexported, and the scheduler work that
// eventually needs a peer to WAIT for capacity must therefore EXTEND this API
// -- a compile-visible change its reviewer meets on the first line of the
// diff, rather than a state that was quietly reachable all along.
type Outcome struct {
	verdict verdict
	reason  outcome.ReasonID
	err     error
	detail  string
	// authorized is the disclosure witness, carried per event. It is never
	// derived from the reason -- see core/outcome.
	authorized bool
	// wire is what the peer is told. Carried, never inferred from the kind.
	wire WireResponse
}

// Continue means the phase is done and the next one may run.
func Continue() Outcome { return Outcome{verdict: verdictContinue} }

// Refuse ends the connection with a declared identity.
func Refuse(id outcome.ReasonID, opts ...OutcomeOption) Outcome {
	o := Outcome{verdict: verdictRefuse, reason: id}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Operational ends the connection because WE failed. It is never the peer's
// doing and never charged to them.
//
// IT CARRIES AN IDENTITY, and the first version did not. That made a registry
// built expressly to give store failures, read failures and other non-refusal
// endings a home unable to see the very outcomes it was created for: they
// passed through the runner unvalidated and reached the record as a bare Go
// error. The raw error stays as bounded operator detail; it is never the
// identity, and it never reaches the peer.
func Operational(id outcome.ReasonID, err error, opts ...OutcomeOption) Outcome {
	o := Outcome{verdict: verdictOperational, reason: id, err: err}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// TerminalControl ends the connection having HANDLED something rather than
// refused it -- the cancel branch, which answers by closing because the peer
// presented no credential and is owed no information, not even whether their
// request landed.
func TerminalControl(id outcome.ReasonID, opts ...OutcomeOption) Outcome {
	o := Outcome{verdict: verdictTerminalControl, reason: id}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WireResponse is what the PEER is told, which is a third question and not a
// consequence of either of the other two.
//
// It used to be inferred: a refusal was assumed to carry a frame and an
// operational ending not to. That is false in both directions -- a TLS failure
// is a refusal with no frame at all, and a store outage is error-driven and
// still owes the caller the uniform denial -- and inferring it is what pushed a
// store outage into being declared a refusal purely because it writes bytes.
//
// The phase states it, because the phase is what knows the protocol state.
type WireResponse uint8

const (
	// WireNothing is the zero value and the safe default: say nothing.
	//
	// A peer mid-TLS cannot read a PostgreSQL frame, and one that has gone
	// away cannot read anything. Silence is also the only answer that cannot
	// disclose something by accident, which is the right default for a value
	// somebody might forget to set.
	WireNothing WireResponse = iota
	// WireUniformDenial is the one shape every refusal shares.
	WireUniformDenial
	// WireFatalInternal is the stable internal error, for an authenticated
	// caller on a quiescent stream.
	WireFatalInternal
)

func (w WireResponse) String() string {
	switch w {
	case WireUniformDenial:
		return "uniform-denial"
	case WireFatalInternal:
		return "fatal-internal"
	}
	return "nothing"
}

// OutcomeOption sets what varies per event rather than per identity.
type OutcomeOption func(*Outcome)

// WithWitness attaches the authorization witness: proof this outcome was
// reached with a verified credential and a checked grant in hand. It is the
// one input that decides whether the surface says more than "denied", so every
// use of it should be visible in review.
func WithWitness() OutcomeOption { return func(o *Outcome) { o.authorized = true } }

// RespondWith states what the peer is told. Absent it, nothing is said, which
// is the only default that cannot disclose anything by accident.
func RespondWith(w WireResponse) OutcomeOption {
	return func(o *Outcome) { o.wire = w }
}

// Wire reports what this outcome says to the peer.
func (o Outcome) Wire() WireResponse { return o.wire }

// WithOutcomeDetail records the internal particular for the operator's record.
func WithOutcomeDetail(detail string) OutcomeOption {
	return func(o *Outcome) { o.detail = detail }
}

// Continues reports whether the lifecycle proceeds past this phase.
func (o Outcome) Continues() bool { return o.verdict == verdictContinue }

// Terminal reports whether this outcome ends the connection.
func (o Outcome) Terminal() bool {
	return o.verdict == verdictRefuse || o.verdict == verdictOperational ||
		o.verdict == verdictTerminalControl
}

// Reason is the declared identity, empty for Continue and Operational.
func (o Outcome) Reason() outcome.ReasonID { return o.reason }

// Err is the underlying failure for an Operational outcome.
func (o Outcome) Err() error { return o.err }

// Detail is the internal particular.
func (o Outcome) Detail() string { return o.detail }

// Witnessed reports whether the authorization witness is present.
func (o Outcome) Witnessed() bool { return o.authorized }

func (o Outcome) String() string {
	switch o.verdict {
	case verdictContinue:
		return "continue"
	case verdictRefuse:
		return "refuse:" + string(o.reason)
	case verdictOperational:
		return fmt.Sprintf("operational:%v", o.err)
	case verdictTerminalControl:
		return "control:" + string(o.reason)
	}
	return "unset"
}

// Phase is one step's declaration.
type Phase struct {
	Name     PhaseName
	Producer outcome.ProducerID
	Cancel   CancelMode
	Replay   ReplayMode
}

// validate refuses a declaration that has not said what it does.
func (p Phase) validate() error {
	if p.Name == "" {
		return fmt.Errorf("frontdoor: a phase with no name")
	}
	if p.Producer == "" {
		return fmt.Errorf("frontdoor: phase %s has no producer, so nothing it emits can be "+
			"checked against what it declared", p.Name)
	}
	if p.Cancel == CancelUnset {
		return fmt.Errorf("frontdoor: phase %s has not said how it responds to cancellation; "+
			"the modes are recorded from the source, and 'not stated' means nobody looked", p.Name)
	}
	if p.Replay == ReplayUnset {
		return fmt.Errorf("frontdoor: phase %s has not said whether it may be re-run; the "+
			"failure mode of guessing is a second reservation or a second session", p.Name)
	}
	return nil
}

// lifecyclePhases is the declaration of the whole lifecycle, read off the
// code rather than aspired to.
func lifecyclePhases() []Phase {
	return []Phase{
		{
			Name: PhaseAccept, Producer: ProducerAccept,
			// Neither: the accept loop's admission is a mutex and some
			// counters. It cannot block, so there is nothing to cancel.
			Cancel: CancelBySocketDeadline,
			// ExactlyOnce: it takes a connection slot, a pre-auth slot and a
			// control-lane reservation. Running it twice for one connection
			// reserves twice and releases once.
			Replay: ExactlyOnce,
		},
		{
			Name: PhaseStartup, Producer: ProducerStartup,
			// The startup exchange is bounded by a deadline set on the
			// connection, not by the listener's context.
			Cancel: CancelBySocketDeadline,
			// It reads from the socket and consumes what it reads; a rerun
			// would be reading the next client's bytes as this one's startup.
			Replay: ExactlyOnce,
		},
		{
			Name: PhaseCancel, Producer: ProducerCancel,
			// CancelByKey takes the listener's context.
			Cancel: CancelByContext,
			// It stops somebody's statement. Twice is not the same as once.
			Replay: ExactlyOnce,
		},
		{
			Name: PhaseAuthenticateAndOpen, Producer: ProducerAuthOpen,
			// The engine call is given a deadline derived from the phase
			// budget and honours it.
			Cancel: CancelByContext,
			// It takes an atomic reservation and publishes a session. A rerun
			// is a second session for one connection.
			Replay: ExactlyOnce,
		},
		{
			Name: PhaseHandshake, Producer: ProducerHandshake,
			// Writing the success sequence is bounded by the socket's
			// deadline.
			Cancel: CancelBySocketDeadline,
			// It registers a cancel key and writes BackendKeyData. A rerun
			// mints a second key for one client.
			Replay: ExactlyOnce,
		},
		{
			Name: PhaseServe, Producer: ProducerServe,
			// The loop reads under the session's own deadlines; its TEARDOWN
			// runs on a context with cancellation dropped, deliberately, so a
			// shutdown does not abandon the cleanup it caused.
			Cancel: CancelDetached,
			Replay: ExactlyOnce,
		},
	}
}
