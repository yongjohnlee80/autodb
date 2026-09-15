package exec

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The engine's post-verification denials, declared in the outcome registry.
//
// THIS IS WHERE THE CHARGE CLASSIFICATION NOW LIVES. It used to be a map in
// this package and nothing but a test connected it to anything; the front door
// asked for a class, the engine answered, and neither could say what the other
// believed. A registry that both consult is what makes "what can happen here,
// and what does it cost the peer" a question with one answer.
//
// The reasons themselves do not move and are not renamed. They adapt: a Deny
// string converts to a ReasonID and back with no value drift, which a cell
// asserts rather than assumes.

// Producer names the engine's wire-session open in the registry.
//
// It is the phase, not the package. Credential verification, the atomic
// reservation, the backend pin and the session publication are ONE call today
// -- splitting them would need its own token, lock and rollback design -- so
// they are one producer, and saying otherwise would describe a structure the
// code does not have.
const Producer = outcome.ProducerID("authenticate-and-open")

// chargeOf maps the engine's own class onto the registry's.
//
// TWO TYPES, ONE MEANING, and the pair is checked by a cell so they cannot
// drift. The engine keeps its own ChargeClass because renaming a type that
// appears across this package and the front door would be a change with no
// behaviour in it -- exactly the kind of noise that makes a refactor
// unreviewable.
func chargeOf(c ChargeClass) outcome.Charge {
	switch c {
	case ChargeCredential:
		return outcome.Credential
	case ChargeProtocol:
		return outcome.Protocol
	case ChargeCapacity:
		return outcome.Capacity
	case ChargeNone:
		return outcome.None
	}
	return outcome.ChargeUnset
}

// classOf is chargeOf's inverse, for a caller reading the registry back.
func classOf(c outcome.Charge) (ChargeClass, bool) {
	switch c {
	case outcome.Credential:
		return ChargeCredential, true
	case outcome.Protocol:
		return ChargeProtocol, true
	case outcome.Capacity:
		return ChargeCapacity, true
	case outcome.None:
		return ChargeNone, true
	}
	// NotApplicable has no engine equivalent and must not be invented one. A
	// statement-level outcome cannot reach the per-source throttle; mapping it
	// onto ChargeNone here would let it be read back as a decision not to
	// charge, which is a different claim.
	return 0, false
}

// Registration declares every post-verification denial this phase can raise.
//
// Built from the declared reasons and their ruled classes rather than from a
// second hand-written list: the exhaustiveness cell already fails the build
// for a reason with no class, so deriving here means the registry cannot be
// less complete than the classification it is built from.
func Registration() outcome.Registration {
	decls := make([]outcome.Decl, 0, len(DenialReasons()))
	for _, r := range DenialReasons() {
		class, ok := denialCharge[r]
		if !ok {
			// UNREACHABLE WHILE THE EXHAUSTIVENESS CELL HOLDS, and it fails
			// loudly rather than registering a guess. A reason that reached
			// here unclassified would otherwise enter the registry with the
			// zero charge, which composition refuses -- a worse error message
			// for the same fault.
			panic(fmt.Sprintf("exec: %q has no ruled charge class; classify it beside the "+
				"others rather than letting the registry guess", r))
		}
		decls = append(decls, outcome.Decl{
			ID:     outcome.ReasonID(r),
			Kind:   outcome.Refusal,
			Charge: chargeOf(class),
		})
	}
	return outcome.Registration{Producer: Producer, Outcomes: decls}
}

// engineRegistry is this package's own composed view, used to answer charge
// questions. Composed once: a failure here is a programming error in the
// declarations above and there is no request to fail instead.
var engineRegistry = mustCompose()

func mustCompose() *outcome.Registry {
	r, err := outcome.Compose(Registration())
	if err != nil {
		panic("exec: the engine's outcome registration does not compose: " + err.Error())
	}
	return r
}

// AdmissionOutcomes is statement admission's side of the registry, one
// producer per stage.
//
// Exported from here because the chain is built here: the stage list is this
// package's, so the daemon composing a registry should not have to know how to
// assemble one to ask what it can emit.
func AdmissionOutcomes() []outcome.Registration { return admissionRegistry.Outcomes() }
