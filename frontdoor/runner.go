package frontdoor

import (
	"fmt"
	"io"
	"sync"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The lifecycle runner: the thing that knows which phase is running, what it
// is allowed to conclude, and whether it may run at all.
//
// IT DRIVES WHAT ALREADY EXISTS. Each phase's body is the code that was
// already there; what is new is that the body now hands back a declared
// Outcome instead of setting a string and falling through, and that handing
// back something undeclared is an error rather than a wire frame.
//
// WHAT IT DELIBERATELY DOES NOT DO is impose a cancellation model, retry
// anything, or decide what reaches the peer. The phases use a mixture of
// socket deadlines, context deadlines and deliberately detached teardown;
// unifying them would be a behaviour change wearing a refactor's clothes, so
// each phase DECLARES what it already does and the runner records it.

// composeListenerOutcomes builds the registry this listener's phases answer
// to: its own declarations, plus the engine's, because the credential phase
// refuses with the engine's identities on the engine's behalf.
//
// The listener composes its own rather than being handed one, so a listener
// built in a test has the same registry as one built by the daemon. A registry
// supplied from outside is a registry a caller can get wrong, and the failure
// would be a phase refusing with an identity that is perfectly valid.
func composeListenerOutcomes() (*outcome.Registry, error) {
	regs := append([]outcome.Registration{}, Outcomes()...)
	return outcome.Compose(append(regs, exec.Registration())...)
}

// lifecycle is one connection's run through the phases.
//
// PER CONNECTION, because the thing it tracks is per connection: whether a
// phase that may run exactly once has already run. A runner shared between
// connections would refuse the second connection's startup on the grounds that
// the first one had a startup.
//
//	  [Incoming Connection]
//	            │
//	            ▼
//	  ┌───────────────────┐
//	  │ lifecycle.run()   │ ──► Verify Phase Known & ReplayMode (ExactlyOnce)
//	  └─────────┬─────────┘
//	            │
//	            ▼
//	       Execute Body
//	            │
//	            ▼
//	      Evaluate Verdict
//	      ├── verdictUnset ──► Fatal Fault (Closed)
//	      ├── verdictContinue ──► Proceed to Next Phase
//	      └── verdictRefuse / Operational / TerminalControl
//	            │
//	            ▼
//	      Resolve Occurrence via Registry (outcome.Occur)
//	      • ProducerID must match phase
//	      • ReasonID must be declared
//	      • Verdict Kind must match Declaration Kind
//
type lifecycle struct {
	reg    *outcome.Registry
	phases map[PhaseName]Phase
	ran    map[PhaseName]bool

	// order records which phases ran AND WHAT EACH CONCLUDED, in order, so a
	// cell can assert the exact prefix a scenario should produce and the
	// verdict each phase reached. Production never reads it.
	//
	// The conclusion is part of it because the prefix alone is not enough: a
	// phase that returns Continue where it should refuse still RAN, so a cell
	// watching only which phases executed cannot tell a recorded refusal from
	// one reconstructed afterwards by somebody else.
	order []phaseRecord
}

// phaseRecord is one phase's execution and its conclusion.
type phaseRecord struct {
	phase   PhaseName
	outcome Outcome
}

func (l *Listener) newLifecycle() *lifecycle {
	// FALLS BACK TO THE PACKAGE'S OWN DECLARATIONS rather than to an empty
	// table. The phases and the vocabulary are properties of this package, not
	// configuration a listener supplies -- Open composes them early so a
	// malformed declaration is caught at start-up, but a Listener built
	// directly (as several cells build one) must still run the same lifecycle.
	//
	// An empty table would make every phase unknown, and an unknown phase does
	// not run its body. That is not a degraded lifecycle, it is a connection
	// that skips startup and authentication while reporting neither.
	reg, phases := l.outcomes, l.phases
	if reg == nil || phases == nil {
		reg, phases = packageLifecycle()
	}
	return &lifecycle{reg: reg, phases: phases, ran: map[PhaseName]bool{}}
}

var (
	pkgLifecycleOnce   sync.Once
	pkgLifecycleReg    *outcome.Registry
	pkgLifecyclePhases map[PhaseName]Phase
)

func packageLifecycle() (*outcome.Registry, map[PhaseName]Phase) {
	pkgLifecycleOnce.Do(func() {
		reg, err := composeListenerOutcomes()
		if err != nil {
			// The declarations are this package's own, so this cannot depend
			// on anything a caller did. It is a programming error that Open
			// would have reported; here there is no caller to report it to.
			panic("frontdoor: the package's outcome declarations do not compose: " + err.Error())
		}
		pkgLifecycleReg = reg
		pkgLifecyclePhases = map[PhaseName]Phase{}
		for _, p := range lifecyclePhases() {
			pkgLifecyclePhases[p.Name] = p
		}
	})
	return pkgLifecycleReg, pkgLifecyclePhases
}

// PhaseResult is what a phase concluded AND the validated occurrence for it.
//
// ONE RESOLUTION, CARRIED. Every consumer -- the throttle, the wire, the record
// -- reads the same value. Each of them used to resolve the identity again
// from whatever field was nearest, and two resolutions of one event is two
// chances to disagree: the credential phase recorded a hard-coded identity
// while the throttle consulted the one the source had actually chosen.
type PhaseResult struct {
	Outcome    Outcome
	Occurrence outcome.Occurrence
}

// Continues reports whether the lifecycle proceeds past this phase.
func (r PhaseResult) Continues() bool { return r.Outcome.Continues() }

// run executes one phase and checks what it concluded.
//
// The error result is a PROGRAMMING error -- an undeclared identity, a rerun
// of something that may happen once, a phase that returned nothing. It is
// separate from the Outcome deliberately: a refusal is a normal thing for a
// phase to conclude and travels in the Outcome, while these are faults in the
// code that no peer caused and none should be told about.
func (lc *lifecycle) run(name PhaseName, body func() Outcome) (PhaseResult, error) {
	p, known := lc.phases[name]
	if !known {
		return PhaseResult{}, fmt.Errorf("frontdoor: %s is not a declared phase", name)
	}
	if lc.ran[name] {
		if p.Replay == ExactlyOnce {
			// REFUSED, NOT PERFORMED. The phases that carry this mode take an
			// atomic reservation, consume bytes off a socket, or publish a
			// session; running one twice for one connection means two
			// reservations against one release, or reading the next message
			// as though it were this one. Nothing should promise a rerun the
			// code cannot deliver, so the promise is declined.
			return PhaseResult{}, fmt.Errorf("frontdoor: %s is exactly-once and has already run", name)
		}
	}
	lc.ran[name] = true

	got := body()
	lc.order = append(lc.order, phaseRecord{phase: name, outcome: got})

	switch got.verdict {
	case verdictUnset:
		// FAILS CLOSED. A phase that returned the zero Outcome fell off the
		// end of its own logic, and the safe reading of "I did not say" is
		// never "carry on" -- carrying on here would hand an unauthenticated
		// connection to the next phase.
		return PhaseResult{}, fmt.Errorf("frontdoor: %s concluded nothing; the zero outcome is not "+
			"a decision to proceed", name)
	case verdictContinue:
		// The only verdict with no identity: nothing happened that anyone
		// needs to be able to name, because the lifecycle simply proceeds.
		return PhaseResult{Outcome: got}, nil
	}

	// EVERY TERMINAL OUTCOME MUST NAME SOMETHING THIS PRODUCER DECLARED --
	// operational endings included, which is what an outcome registry is for. Checking it here, where the producer is still known, is what
	// makes "what can happen in this phase" an answerable question -- and it
	// is the check that stops a reason invented at a call site from arriving
	// at the renderer looking exactly like a declared one, carrying whatever
	// charge the renderer defaults to.
	occ, err := lc.reg.Occur(p.Producer, got.reason, witnessOpts(got)...)
	if err != nil {
		return PhaseResult{}, err
	}

	// THE VERDICT AND THE REGISTERED KIND MUST AGREE.
	//
	// Occur checked WHO may emit the identity and never WHAT KIND of thing it
	// is, so a phase could Refuse with an identity registered as operational,
	// or hand a control identity to Operational, and the registry would
	// cheerfully return the declared kind for it. The record would then say a
	// refusal happened where the declaration says our own failure did -- and
	// the charge travels with the declaration, not the verdict, so the two
	// disagreeing is how an ending gets classified as something it is not.
	if want := kindFor(got.verdict); want != occ.Kind {
		return PhaseResult{}, fmt.Errorf("frontdoor: %s ended %q as %s, but it is registered "+
			"as %s; the verdict and the declaration disagree about what happened",
			name, got.reason, want, occ.Kind)
	}
	return PhaseResult{Outcome: got, Occurrence: occ}, nil
}

// kindFor is the constructor's own claim about what kind of thing happened.
func kindFor(v verdict) outcome.Kind {
	switch v {
	case verdictRefuse:
		return outcome.Refusal
	case verdictOperational:
		return outcome.Operational
	case verdictTerminalControl:
		return outcome.Control
	}
	return outcome.KindUnset
}

func witnessOpts(o Outcome) []outcome.OccurOption {
	var opts []outcome.OccurOption
	if o.authorized {
		opts = append(opts, outcome.Authorized())
	}
	if o.detail != "" {
		opts = append(opts, outcome.WithDetail(o.detail))
	}
	return opts
}

// NOTE: denialOccurrence, chargeFor and lifecycle.occurrence are deliberately
// GONE, all three.
//
// Each resolved an identity a second time, from whatever field was nearest,
// for consumers that already had a validated one. Two resolutions of one event
// is two chances to disagree, and they did: the credential phase recorded a
// hard-coded identity while the throttle consulted the one the source had
// actually chosen.
//
// The last of them survived as a "just for tests" helper, which is how the
// shape stays available: a cell written against it asserts a projection
// production no longer performs, and the next author reads that cell as
// evidence the path exists. Every consumer reads the PhaseResult its phase
// returned; a cell that wants an occurrence runs the phase, and one that wants
// the registry calls Occur directly.

// ranPhases reports the phases this connection ran, in order.
func (lc *lifecycle) ranPhases() []PhaseName {
	out := make([]PhaseName, 0, len(lc.order))
	for _, r := range lc.order {
		out = append(out, r.phase)
	}
	return out
}

// concluded reports what a phase concluded, and whether it ran at all.
func (lc *lifecycle) concluded(name PhaseName) (Outcome, bool) {
	for _, r := range lc.order {
		if r.phase == name {
			return r.outcome, true
		}
	}
	return Outcome{}, false
}

// lifecycleFault is the ONE place a fault in the runner itself is turned into
// a record and an answer.
//
// IT GOES THROUGH THE REGISTRY LIKE EVERYTHING ELSE. The identity was declared
// under a producer of its own and then emitted by hand at one site and not at
// all at the others, so the one outcome class that describes OUR defects was
// the one class never validated. A declared identity that production emits
// without resolving is a row that proves nothing.
//
// WHAT THE PEER IS TOLD DEPENDS ON WHERE WE ARE, because the protocol does:
//
//   - Before the credential exchange there is nothing to say and often no way
//     to say it -- a peer mid-TLS cannot read a PostgreSQL frame -- so the
//     connection closes.
//   - During it, the peer is owed the same uniform denial every other refusal
//     gives them; silence there turns our one-line declaration mistake into a
//     client hanging on a read.
//   - After session open, on a QUIESCENT stream, they get a stable fatal
//     internal error. Quiescent matters: injecting a frame into a stream that
//     is mid-exchange corrupts it, and a corrupted stream is a worse answer
//     than a closed one.
//
// Nothing here is charged. The peer did nothing.
type faultStage uint8

const (
	// faultBeforeCredential is anything up to and including startup.
	faultBeforeCredential faultStage = iota
	// faultDuringCredential is the credential exchange, where the uniform
	// denial is both available and owed.
	faultDuringCredential
	// faultAfterSessionOpen is a served connection.
	faultAfterSessionOpen
)

func (l *Listener) lifecycleFault(lc *lifecycle, phase PhaseName, peer string, stage faultStage,
	stream io.Writer, quiescent bool, cause error) {

	// RESOLVED ONCE, through the registry, under the producer that owns it.
	// EVERY CALLBACK HERE IS GUARDED SEPARATELY.
	//
	// These are host-supplied funcs, and this path runs where there is no
	// recovery boundary above it: the ACCEPT fault fires on the accept
	// goroutine, before any handler exists, so a panicking application logger
	// escapes Serve itself and takes the listener down -- which is the outage
	// the whole containment mechanism exists to prevent, reached through the
	// code that reports our own defects.
	//
	// Individually, not as a group: a broken logger must not swallow the
	// event, and a broken observer must not swallow the log. Losing both would
	// leave a fault invisible; losing one leaves it half-reported, which is
	// strictly better and is all that is on offer once a callback misbehaves.
	safely(func() {
		l.onLog(fmt.Sprintf("frontdoor: the %s phase for %s: %v", phase, peer, cause))
	})

	occ, err := lc.reg.Occur(ProducerLifecycle, outcomeID(OutcomeInternalError))
	if err != nil {
		// NO EVENT. The identity is this package's own and declared in this
		// package, so failing to resolve it means the declarations are broken
		// -- and an identity we cannot validate must not enter the audit
		// vocabulary, because an unvalidated identity in the trail is exactly
		// what the registry exists to make impossible. The operator gets the
		// whole of it in the log; the connection still ends, above.
		safely(func() {
			l.onLog(fmt.Sprintf("frontdoor: the lifecycle fault identity does not resolve: %v", err))
		})
	} else {
		safely(func() {
			l.onEvent(Event{Kind: EventLifecycleFault, Reason: string(occ.Reason), Peer: peer,
				Detail: string(phase)})
		})
	}
	if occ.Charges() {
		// Unreachable while internal-error is registered None, and asserted
		// rather than assumed: charging a peer for our defect is the failure
		// this whole vocabulary exists to stop.
		safely(func() { l.onLog("frontdoor: refusing to charge a peer for a lifecycle fault") })
	}

	switch stage {
	case faultDuringCredential:
		// THE WRITE IS OUTSIDE THE GUARD, deliberately: it is our own code on
		// a socket, not a host callback, and swallowing its failure would hide
		// a broken stream behind a mechanism meant for broken observers.
		if derr := l.denyWithOccurrence(stream,
			outcome.Occurrence{Reason: outcome.ReasonID(reasonPreAuthProtocolViolation)}); derr != nil {
			safely(func() {
				l.onLog(fmt.Sprintf("frontdoor: writing the denial to %s: %v", peer, derr))
			})
		}
	case faultAfterSessionOpen:
		if !quiescent {
			// A frame injected into a stream mid-exchange corrupts it. The
			// close is the honest answer.
			return
		}
		if derr := sendFatalInternal(stream); derr != nil {
			safely(func() {
				l.onLog(fmt.Sprintf("frontdoor: writing the internal error to %s: %v", peer, derr))
			})
		}
	}
}

// outcomeID converts a phase's own reason string to the registry's neutral
// identity. It is a conversion and not a lookup: the value does not change,
// which is what "the existing types adapt" means and what a cell asserts.
func outcomeID(reason string) outcome.ReasonID { return outcome.ReasonID(reason) }
