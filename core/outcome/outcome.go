// Package outcome is the one place that says what can happen, and who can say
// it.
//
// THREE VOCABULARIES EXISTED AND NONE OF THEM WAS A REGISTRY. Statement
// admission has its codes, the front door has its private denial reasons, and
// the engine has its own strings. Each is hand-maintained, each is complete by
// somebody's care rather than by construction, and nothing connected them --
// so a cancel request, a TLS read error and an operational note belonged to no
// vocabulary at all and lived as bare strings at the call site.
//
// That is how a charge goes missing. A reason nobody classified was charged by
// default, which is how a developer who ran out of capacity was banned for
// grinding credentials.
//
// WHAT THIS IS NOT is a renderer. Nothing here maps a reason to a wire code,
// because the same capacity identity is 28000 without the authorization
// witness and 53300 with it, and accept-phase outcomes have no frame at all. A
// static reason-to-code table would have to pick one and would be wrong for
// the other. Framing is projected from a runtime Occurrence, which carries the
// witness; this package owns IDENTITY, and identity alone.
package outcome

import (
	"fmt"
	"sort"
	"strings"
)

// ReasonID is a neutral identity.
//
// NEUTRAL SO NOTHING HAS TO BE RENAMED. The three existing types adapt to it
// and keep their own values: admission.Code, the front door's denialReason and
// the engine's reason strings all convert, and a cell asserts that no value
// drifts in the conversion. A registry that required a rename would be a
// registry nobody adopted.
type ReasonID string

// ProducerID names the phase or stage that is declaring.
//
// MEMBERSHIP IS THE PRODUCER'S, NOT THE REASON'S, and this is the correction
// that makes the shape work at all. An earlier design put a single phase on
// the reason, which cannot be right: the engine RAISES a capacity refusal and
// the front door RENDERS it, so one identity genuinely has two producers. A
// field on the reason forces one of them to lie.
type ProducerID string

// Kind says what sort of thing happened. A refusal is not the only outcome a
// phase produces, and calling this a refusal registry is what left cancels and
// read errors homeless.
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ Kind: Structural Outcome Category                           │
//	├─────────────┬───────────────────────────────────────────────┤
//	│ Refusal     │ Decision not to proceed (e.g. invalid syntax) │
//	│ Control     │ Protocol lifecycle action (e.g. cancel frame) │
//	│ Operational │ Error-driven ending (e.g. connection broken)  │
//	│ Note        │ Informative observation (e.g. parameter reset)│
//	└─────────────┴───────────────────────────────────────────────┘
type Kind uint8

const (
	// KindUnset is the zero value and is REJECTED at composition. A
	// declaration that forgot to say what it was would otherwise register as
	// whichever kind happened to be first, silently.
	KindUnset Kind = iota
	// Refusal is a decision not to proceed, which the peer may be told about
	// in whatever shape the renderer chooses.
	Refusal
	// Control is a protocol action that is not a refusal: a cancel request
	// arriving, a terminal branch that never authenticates.
	Control
	// Operational is an ENDING DRIVEN BY AN ERROR rather than by a decision:
	// a read that broke, a store that would not answer, a peer that went away
	// mid-exchange.
	//
	// IT SAYS NOTHING ABOUT ATTRIBUTION, and the first version of this comment
	// said it did -- "never the peer's doing and never charged". That was
	// wrong twice over. A peer abandoning the credential exchange is an
	// error-driven ending AND squarely theirs; a store outage is error-driven
	// and squarely ours. Reading attribution off the kind forced one of those
	// two to be misfiled, and the fix was to reclassify one of them by what
	// the WIRE does -- which is a third question again.
	//
	// So: Kind says what sort of ending it was. Charge alone says who is
	// answerable. What the peer is told is decided by protocol state at the
	// projection, not here. Three questions, three answers, and none of them
	// may be inferred from another.
	Operational
	// Note is an observation recorded beside an accepted outcome, such as a
	// startup parameter that was adjusted rather than refused.
	Note
)

func (k Kind) String() string {
	switch k {
	case Refusal:
		return "refusal"
	case Control:
		return "control"
	case Operational:
		return "operational"
	case Note:
		return "note"
	}
	return "unset"
}

// Charge is what this outcome costs the peer's source address.
//
// THE SOLE ANSWER TO ATTRIBUTION, independent of Kind. An error-driven ending
// may be the peer's doing or ours, and a decision may be either; nothing about
// one axis constrains the other.
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ Charge: Throttle Attribution Category                       │
//	├───────────────┬───────────────────────┬─────────────────────┤
//	│ Charge Class  │ Who Is Answerable?    │ Increments Throttle?│
//	├───────────────┼───────────────────────┼─────────────────────┤
//	│ Credential    │ Peer (Bad Secret)     │ YES (Charges IP)    │
//	│ Protocol      │ Peer (Bad Framing)    │ YES (Charges IP)    │
//	│ Capacity      │ System (Out of Space) │ NO  (Never Charged) │
//	│ None          │ System (Internal Err) │ NO  (Never Charged) │
//	│ NotApplicable │ Out of Throttle Scope │ NO  (Never Charged) │
//	└───────────────┴───────────────────────┴─────────────────────┘
type Charge uint8

const (
	// ChargeUnset is the zero value and is REJECTED at composition, for the
	// same reason KindUnset is: the failure mode of a missing charge is that
	// something gets charged by accident, and that is the failure that banned
	// a developer.
	ChargeUnset Charge = iota
	// Credential: the peer presented something wrong.
	Credential
	// Protocol: the peer spoke the protocol wrongly, or failed a handshake.
	// Charged, because an attacker who could switch from credential grinding
	// to handshake grinding for a fresh allowance would have no reason not to.
	Protocol
	// Capacity: WE ran out. Never charged. Charging it is what turned a full
	// connection pool into a banned source address.
	Capacity
	// None: our configuration, our stored state, our bug. Never charged --
	// each of these follows a verified credential, so the caller has already
	// proved who they are and what they met is a fact about what we stored.
	None
	// NotApplicable: this outcome cannot reach the per-source throttle at all.
	//
	// DISTINCT FROM None, and the distinction is the point. A statement-level
	// refusal happens on an authenticated session that is already past every
	// accept-time budget; there is no per-source counter in reach. Recording
	// that as "none" would say a decision was made not to charge it, when in
	// truth the question does not arise.
	NotApplicable
)

// Charges reports whether this class counts against the credential throttle.
func (c Charge) Charges() bool { return c == Credential || c == Protocol }

func (c Charge) String() string {
	switch c {
	case Credential:
		return "credential"
	case Protocol:
		return "protocol"
	case Capacity:
		return "capacity"
	case None:
		return "none"
	case NotApplicable:
		return "not-applicable"
	}
	return "unset"
}

// Decl is one identity as one producer declares it.
type Decl struct {
	ID     ReasonID
	Kind   Kind
	Charge Charge
}

// Registration is everything one producer can emit -- not its refusals alone.
type Registration struct {
	Producer ProducerID
	Outcomes []Decl
}

// Registry is the composed result: every identity, with the metadata every
// producer agreed on, and who declared it.
type Registry struct {
	decls     map[ReasonID]Decl
	producers map[ReasonID][]ProducerID
	byPair    map[pair]struct{}
}

type pair struct {
	producer ProducerID
	reason   ReasonID
}

// Compose builds the registry, or explains why it cannot.
//
// EVERY RULE HERE FAILS THE BUILD RATHER THAN THE REQUEST. A registry that
// resolved a conflict at emit time would resolve it differently depending on
// which producer registered last, which is a coin toss wearing a data
// structure. The cost of composing wrongly is paid once, at start-up, by the
// person who can fix it.
func Compose(regs ...Registration) (*Registry, error) {
	r := &Registry{
		decls:     map[ReasonID]Decl{},
		producers: map[ReasonID][]ProducerID{},
		byPair:    map[pair]struct{}{},
	}
	for _, reg := range regs {
		if reg.Producer == "" {
			return nil, fmt.Errorf("outcome: a registration with no producer; membership is the "+
				"producer's, so an anonymous one declares nothing it can be held to (%d outcome(s))",
				len(reg.Outcomes))
		}
		for _, d := range reg.Outcomes {
			if err := r.add(reg.Producer, d); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

func (r *Registry) add(p ProducerID, d Decl) error {
	if d.ID == "" {
		return fmt.Errorf("outcome: %s declared an outcome with no identity", p)
	}
	if d.Kind == KindUnset {
		return fmt.Errorf("outcome: %s declared %q with no kind; a cancel, a read error and a "+
			"refusal are different things and the zero value is not one of them", p, d.ID)
	}
	if d.Charge == ChargeUnset {
		return fmt.Errorf("outcome: %s declared %q with no charge class; an unclassified reason "+
			"is one nobody has reasoned about, and the default that used to apply to it is what "+
			"banned a developer for running out of capacity", p, d.ID)
	}
	k := pair{p, d.ID}
	if _, dup := r.byPair[k]; dup {
		// ONE PRODUCER, ONE DECLARATION. Two declarations of the same
		// identity by the same producer are either a copy-paste or a
		// disagreement with itself, and the second is the dangerous one.
		return fmt.Errorf("outcome: %s declared %q twice", p, d.ID)
	}
	if prev, seen := r.decls[d.ID]; seen && (prev.Kind != d.Kind || prev.Charge != d.Charge) {
		// SEVERAL PRODUCERS MAY SHARE AN IDENTITY -- the engine raises a
		// capacity refusal and the front door renders it -- but they must
		// agree about what it IS. Letting the last registration win would
		// make the charge depend on composition order.
		return fmt.Errorf("outcome: %q is declared as %s/%s by %s and as %s/%s by %s; "+
			"an identity means one thing or it is two identities",
			d.ID, prev.Kind, prev.Charge, strings.Join(producerNames(r.producers[d.ID]), ", "),
			d.Kind, d.Charge, p)
	}
	r.byPair[k] = struct{}{}
	r.decls[d.ID] = d
	r.producers[d.ID] = append(r.producers[d.ID], p)
	return nil
}

func producerNames(ps []ProducerID) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// Lookup returns the declared metadata for an identity.
//
// The second result is false for an identity nobody declared. A caller must
// treat that as a failure rather than a default: an undeclared reason is the
// state this package exists to make impossible, and inventing metadata for it
// would restore the behaviour that lost the charge in the first place.
func (r *Registry) Lookup(id ReasonID) (Decl, bool) {
	d, ok := r.decls[id]
	return d, ok
}

// Producers reports who declared an identity, in registration order.
func (r *Registry) Producers(id ReasonID) []ProducerID {
	return append([]ProducerID(nil), r.producers[id]...)
}

// Reasons lists every declared identity, sorted, so a walk over the registry
// is stable enough to assert on.
func (r *Registry) Reasons() []ReasonID {
	out := make([]ReasonID, 0, len(r.decls))
	for id := range r.decls {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
