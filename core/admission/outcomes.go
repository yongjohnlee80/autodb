package admission

import "github.com/yongjohnlee80/autodb/core/outcome"

// Outcomes projects the chain's existing per-stage declarations into the
// outcome registry, one producer per STAGE.
//
// DERIVED, NOT RESTATED. Every stage already declares the codes it can deny
// with -- disclosure is mandatory on Stage itself, so a stage added later
// cannot escape it -- and a hand-written list beside that would be a second
// copy of a fact the chain already owns. Two copies is how a registry ends up
// describing a version of the system that no longer exists.
//
// One producer per stage rather than one for the package, because that is what
// the registry's membership question means: "what can happen HERE" is a
// question about a stage, and collapsing twelve stages into one producer makes
// it unanswerable.
//
// EVERY CODE IS NotApplicable, WHICH IS NOT None. These decisions are made on
// an authenticated session already past every accept-time budget; there is no
// per-source counter in reach. "None" would claim somebody decided not to
// charge them, when the question does not arise.
func (o *Orchestrator) Outcomes() []outcome.Registration {
	out := make([]outcome.Registration, 0, len(o.stages))
	for _, reg := range o.Registered() {
		decls := make([]outcome.Decl, 0, len(reg.DenyCod))
		for _, c := range reg.DenyCod {
			decls = append(decls, outcome.Decl{
				ID:     outcome.ReasonID(c),
				Kind:   outcome.Refusal,
				Charge: outcome.NotApplicable,
			})
		}
		if len(decls) == 0 {
			// An observer stage declares nothing and registers nothing. It is
			// not an error: a stage that cannot refuse has no identities to
			// own, and inventing an empty producer would put a name in the
			// registry that answers no question.
			continue
		}
		out = append(out, outcome.Registration{Producer: outcome.ProducerID(reg.Name), Outcomes: decls})
	}
	return out
}
