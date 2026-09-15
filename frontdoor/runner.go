package frontdoor

import (
	"fmt"
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

// run executes one phase and checks what it concluded.
//
// The error result is a PROGRAMMING error -- an undeclared identity, a rerun
// of something that may happen once, a phase that returned nothing. It is
// separate from the Outcome deliberately: a refusal is a normal thing for a
// phase to conclude and travels in the Outcome, while these are faults in the
// code that no peer caused and none should be told about.
func (lc *lifecycle) run(name PhaseName, body func() Outcome) (Outcome, error) {
	p, known := lc.phases[name]
	if !known {
		return Outcome{}, fmt.Errorf("frontdoor: %s is not a declared phase", name)
	}
	if lc.ran[name] {
		if p.Replay == ExactlyOnce {
			// REFUSED, NOT PERFORMED. The phases that carry this mode take an
			// atomic reservation, consume bytes off a socket, or publish a
			// session; running one twice for one connection means two
			// reservations against one release, or reading the next message
			// as though it were this one. Nothing should promise a rerun the
			// code cannot deliver, so the promise is declined.
			return Outcome{}, fmt.Errorf("frontdoor: %s is exactly-once and has already run", name)
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
		return Outcome{}, fmt.Errorf("frontdoor: %s concluded nothing; the zero outcome is not "+
			"a decision to proceed", name)
	case verdictContinue:
		// The only verdict with no identity: nothing happened that anyone
		// needs to be able to name, because the lifecycle simply proceeds.
		return got, nil
	}

	// EVERY TERMINAL OUTCOME MUST NAME SOMETHING THIS PRODUCER DECLARED --
	// operational endings included, which is what an outcome registry is for. Checking it here, where the producer is still known, is what
	// makes "what can happen in this phase" an answerable question -- and it
	// is the check that stops a reason invented at a call site from arriving
	// at the renderer looking exactly like a declared one, carrying whatever
	// charge the renderer defaults to.
	if _, err := lc.reg.Occur(p.Producer, got.reason, witnessOpts(got)...); err != nil {
		return Outcome{}, err
	}
	return got, nil
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

// occurrence projects a phase's outcome into the registry's runtime value, for
// the record. It is the ONLY way an occurrence is built, so the witness cannot
// be attached anywhere the phase did not attach it.
func (lc *lifecycle) occurrence(name PhaseName, o Outcome) (outcome.Occurrence, error) {
	p, known := lc.phases[name]
	if !known {
		return outcome.Occurrence{}, fmt.Errorf("frontdoor: %s is not a declared phase", name)
	}
	return lc.reg.Occur(p.Producer, o.reason, witnessOpts(o)...)
}

// outcomeID converts a phase's own reason string to the registry's neutral
// identity. It is a conversion and not a lookup: the value does not change,
// which is what "the existing types adapt" means and what a cell asserts.
func outcomeID(reason string) outcome.ReasonID { return outcome.ReasonID(reason) }

// denialOccurrence resolves the typed occurrence for a refusal that is about
// to be rendered.
//
// FAILS CLOSED. If the identity cannot be resolved -- an undeclared reason, the
// wrong producer -- the zero occurrence is returned: no witness and no charge,
// which the projection can only render as the uniform denial. A refusal we
// cannot classify is precisely the one that must not be allowed to disclose
// anything, and returning the reason with disclosure intact would let a
// registration mistake become a capacity oracle.
func (l *Listener) denialOccurrence(lc *lifecycle, phase PhaseName, reason denialReason, witness bool) outcome.Occurrence {
	opts := []OutcomeOption{}
	if witness {
		opts = append(opts, WithWitness())
	}
	occ, err := lc.occurrence(phase, Refuse(outcomeID(reason.String()), opts...))
	if err != nil {
		l.onLog(fmt.Sprintf("frontdoor: %s produced an unresolvable denial %q: %v", phase, reason, err))
		return outcome.Occurrence{Reason: outcome.ReasonID(reason)}
	}
	return occ
}

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

// chargeFor applies the throttle for a non-denial ending, from the identity's
// REGISTERED class and nothing else.
//
// An identity that cannot be resolved is not charged. A charge is a real cost
// to a real developer -- ten of them bans their address for a minute -- and
// charging one because our own registration was wrong is the failure this
// whole vocabulary exists to stop.
func (l *Listener) chargeFor(lc *lifecycle, phase PhaseName, id outcome.ReasonID, peer string) {
	if id == "" {
		return
	}
	occ, err := lc.occurrence(phase, Outcome{verdict: verdictOperational, reason: id})
	if err != nil {
		l.onLog(fmt.Sprintf("frontdoor: %s produced an unclassifiable ending %q: %v", phase, id, err))
		return
	}
	if occ.Charges() {
		l.admit.noteFailure(peer)
	}
}
