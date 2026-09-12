package admission

import (
	"errors"
	"fmt"
	"strings"
)

// OperationalError means an admission stage could not answer its policy
// question. It is not a refusal: surfaces keep the wrapped cause server-side
// and project a stable opaque operational response to the client.
type OperationalError struct {
	Stage string
	Cause error
}

// Error implements the error interface for OperationalError.
func (e *OperationalError) Error() string {
	return fmt.Sprintf("admission: stage %s broke: %v", e.Stage, e.Cause)
}

// Unwrap returns the underlying cause of the operational error.
func (e *OperationalError) Unwrap() error { return e.Cause }

// IsOperationalError reports whether a pipeline evaluation broke rather than
// producing a refusal.
func IsOperationalError(err error) bool {
	var operational *OperationalError
	return errors.As(err, &operational)
}

// Orchestrator composes stages into one ordered chain and evaluates a
// statement through it.
//
// The chain is DATA: composed once per profile, rendered for review and
// assertion, and identical for a connection across all frontends—differing
// only where a stage is physically inapplicable, never in SQL policy.
//
//	+-----------------------------------------------------------------------+
//	|                         Orchestrator Chain                            |
//	+-----------------------------------------------------------------------+
//	| Stage 1: Size Intake Bound     (script-too-large)                     |
//	|    │                                                                  |
//	|    ▼                                                                  |
//	| Stage 2: Predicate Guard       (mutation-without-predicate)           |
//	|    │                                                                  |
//	|    ▼                                                                  |
//	| Stage 3: Routine Catalog Check (reader-advanced-pattern)              |
//	|    │                                                                  |
//	|    ▼                                                                  |
//	| Stage 4: Session State & GUCs  (set-guc-refused, lock-outside-tx)     |
//	+-----------------------------------------------------------------------+
//
// THE CHAIN STOPS AT THE FIRST DENY: This is a fundamental design property
// rather than an economy. The first refusal is the most fundamental one,
// and caller remediation is simpler when addressing the root violation
// rather than confusing multiple secondary diagnostic messages.
type Orchestrator struct {
	stages []Stage
}

// Compose builds an orchestrator from an ordered stage list. The order IS
// the policy: size before understanding, capability before reader
// analysis, the guard after the class floor. Reordering is a reviewable
// change asserted by the order cells, not a silent edit.
func Compose(stages ...Stage) *Orchestrator {
	names := map[string]bool{}
	for _, s := range stages {
		if names[s.Name()] {
			panic("admission: duplicate stage name " + s.Name())
		}
		names[s.Name()] = true
	}
	return &Orchestrator{stages: stages}
}

// Run evaluates one statement's facts through the chain in sequence.
//
// Execution terminates on the first stage that emits a denial, or aborts
// if an operational error occurs:
//
//	             Orchestrator.Run(facts, ctx)
//	                          │
//	            ┌─────────────▼─────────────┐
//	     ┌─────►│ Pick Next Stage in Chain  │
//	     │      └─────────────┬─────────────┘
//	     │                    │
//	     │      ┌─────────────▼─────────────┐
//	     │      │ Is Stage Applicable?      │
//	     │      │ (Check Needs vs Context)  │
//	     │      └──────┬─────────────┬──────┘
//	     │          No │             │ Yes
//	     │             │             ▼
//	     │             │     ┌───────────────────────────┐
//	     │             │     │ Stage.Apply(facts, ctx)   │
//	     │             │     └─────────────┬─────────────┘
//	     │             │                   │
//	     │             │     ┌─────────────▼─────────────┐
//	     │             │     │ Operational Error?        │
//	     │             │     └──────┬─────────────┬──────┘
//	     │             │        Yes │             │ No
//	     │             │            │             ▼
//	     │             │            │     ┌───────────────────────────┐
//	     │             │            │     │ contrib.Deny != nil?      │
//	     │             │            │     └──────┬─────────────┬──────┘
//	     │             │        Abort │      Yes │             │ No
//	     │             │      Pipeline│          │             ▼
//	     │             │              │          │     ┌───────────────────────────┐
//	     │             │              │          │     │ Record Risk Observation   │
//	     │             │              │          │     └─────────────┬─────────────┘
//	     │             │              │          │                   │
//	     │             └──────────────┼──────────┼───────────────────┘
//	     │                            │          │
//	     ▼                            ▼          ▼
//	(End of Chain)              ABORT RUN     STOP PIPELINE
//	Return Report (Admitted)  OperationalErr  Return Report (Deny)
//
// A DENY SUPPRESSES RISK: When a stage emits both a denial and an observation,
// the denial takes precedence and risk is dropped. The primary analytical
// record of a denied statement is its denial reason.
func (o *Orchestrator) Run(facts Facts, ctx Context) (Report, error) {
	var rep Report
	for _, s := range o.stages {
		if !applicable(s, facts, ctx) {
			continue
		}
		contrib, err := s.Apply(facts, ctx)
		if err != nil {
			return Report{}, &OperationalError{Stage: s.Name(), Cause: err}
		}
		if contrib.Deny != nil {
			// AN UNDECLARED DENIAL IS A DISCLOSURE VIOLATION, rejected
			// rather than honored. The registration walks that protect the
			// renderer and the record derive their obligations from
			// DenyCodes; a denial the stage never declared is a refusal the
			// pipeline's consumers cannot know about — honoring it would
			// silently exempt it from every obligation the seam promises.
			if !declaresCode(s, contrib.Deny.Code) {
				return Report{}, &OperationalError{Stage: s.Name(), Cause: fmt.Errorf(
					"denied with undeclared code %q — declare it in DenyCodes or do not deny with it",
					contrib.Deny.Code)}
			}
			rep.Deny = append(rep.Deny, *contrib.Deny)
			return rep, nil
		}
		if contrib.Risk != nil {
			rep.Risk = append(rep.Risk, *contrib.Risk)
		}
	}
	return rep, nil
}

// applicable reports whether the stage's declared needs are satisfied by
// this evaluation's facts and context. Fact APPLICABILITY is decided here
// as well as context applicability: a stage requiring a set shape on a
// chain whose facts carry none is unsatisfiable, and the composition says
// so rather than the stage returning nothing at runtime.
func applicable(s Stage, facts Facts, ctx Context) bool {
	n := s.ContextNeeds()
	if n.ReadOnlyUnit && !ctx.ReadOnly {
		return false
	}
	if n.ControlVerb && facts.Class() != ClassControl {
		return false
	}
	if n.SetShape {
		if _, _, ok := facts.SetTarget(); !ok {
			return false
		}
	}
	// ONSESSION IS AFFIRMATIVE, never "not pooled". The zero value of
	// PhysicalCtx — and any invalid value a future edit could introduce —
	// must NOT satisfy a session-only stage: a transport boundary that
	// admits unknown contexts into session-only gates is the shape of a
	// security hole, which is why the check asks "is it session or wire"
	// rather than "is it not pooled".
	if n.OnSession && ctx.Phys != PhysSession && ctx.Phys != PhysWire {
		return false
	}
	// Target capabilities are applicability too: a stage requiring what
	// the target cannot do is absent by construction, rather than
	// discovering the absence in Apply and returning empty.
	if !ctx.TargetCaps.Has(n.TargetCaps) {
		return false
	}
	return true
}

// Order returns the chain's stage names in declared order — the value the
// order cells assert against.
func (o *Orchestrator) Order() []string {
	out := make([]string, 0, len(o.stages))
	for _, s := range o.stages {
		out = append(out, s.Name())
	}
	return out
}

// Render draws the chain as a reviewable line: profile-agnostic, in
// order, with inapplicability decided per composition rather than hidden
// in branches. The v1compat/session difference is a rendering you can
// diff; maintaining a policy is archaeology without one.
func (o *Orchestrator) Render() string {
	return strings.Join(o.Order(), " → ")
}

// Registration describes one chain stage for the discovery consumers.
type Registration struct {
	Name    string
	DenyCod []Code // every Code the stage can deny with
}

// Registered exposes the chain's stages and their DECLARED deny codes —
// the single disclosure source for the renderer-completeness walk and the
// record-on-every-refusal enumeration. Disclosure is mandatory on Stage
// itself (an observer declares nil), so every stage registers and a stage
// added in a later phase cannot silently escape either walk.
func (o *Orchestrator) Registered() []Registration {
	out := make([]Registration, 0, len(o.stages))
	for _, s := range o.stages {
		out = append(out, Registration{Name: s.Name(), DenyCod: s.DenyCodes()})
	}
	return out
}

// declaresCode reports whether the stage declared the given code.
func declaresCode(s Stage, c Code) bool {
	for _, d := range s.DenyCodes() {
		if d == c {
			return true
		}
	}
	return false
}
