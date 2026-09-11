package admission

import (
	"fmt"
	"strings"
)

// Orchestrator composes stages into one ordered chain and evaluates a
// statement through it. The chain is DATA: composed once per profile,
// rendered for review and assertion, and identical for a connection on
// every surface — differing only where a stage is physically
// inapplicable, never in SQL policy.
//
// THE CHAIN STOPS AT THE FIRST DENY, and that is a design property rather
// than an economy: the first refusal is the fundamental one (the order is
// declared so it stays that way — the most fundamental ground first), and
// a caller told three things at once learns the wrong one first.
//
// THE REGISTRATION IS THE DISCLOSURE SOURCE. Registered() exposes the
// chain's stage names and every Code a stage can deny with, so the
// renderer-completeness walk and the record-on-every-refusal enumeration
// DISCOVER their obligations from the live chain rather than from a
// hand-maintained list — one mechanism, two consumers, and a stage added
// in a later phase cannot silently escape either.
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

// Run evaluates one statement's facts through the chain, in order, and
// stops at the first deny. Stages whose declared needs the context cannot
// supply are skipped — not consulted, not logged as empty, skipped —
// because their absence is a property of the composition, decided when the
// chain was built, not a runtime surprise.
//
// An operational error from a stage ABORTS the run and is returned as
// itself. It is not a refusal: the caller must be able to tell "the
// statement is refused" from "the pipeline could not decide", and the
// Report type gives it that.
func (o *Orchestrator) Run(facts Facts, ctx Context) (Report, error) {
	var rep Report
	for _, s := range o.stages {
		if !applicable(s, facts, ctx) {
			continue
		}
		contrib, err := s.Apply(facts, ctx)
		if err != nil {
			return Report{}, fmt.Errorf("admission: stage %s broke: %w", s.Name(), err)
		}
		if contrib.Deny != nil {
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
	if n.OnSession && ctx.Phys == PhysPooled {
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

// Registered exposes the chain's stages and their deny codes — the single
// disclosure source for the renderer-completeness walk and the
// record-on-every-refusal enumeration. A stage added later extends this
// automatically; the walks fail until their obligations cover it, which is
// the point of sharing the mechanism.
func (o *Orchestrator) Registered() []Registration {
	out := make([]Registration, 0, len(o.stages))
	for _, s := range o.stages {
		if d, ok := s.(Denier); ok {
			out = append(out, Registration{Name: s.Name(), DenyCod: d.DenyCodes()})
			continue
		}
		out = append(out, Registration{Name: s.Name()})
	}
	return out
}

// Denier is the OPTIONAL disclosure a denying stage implements: every Code
// it can deny with. It is optional because a stage that never denies (the
// A15 fake, a pure risk observer) has nothing to disclose; it is an
// interface rather than a Stage method because the registration mechanism
// must work for ANY Stage without forcing every future observer to
// implement a vestigial arm.
type Denier interface {
	DenyCodes() []Code
}
