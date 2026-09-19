package outcome

import "fmt"

// An Occurrence is one outcome actually happening, as opposed to one that
// could.
//
// IT EXISTS BECAUSE THE REGISTRY MUST NOT RENDER. The obvious design is a
// table from reason to wire code, and it breaks on the first row: a capacity
// refusal is 28000 to a stranger and 53300 to a caller who has already proved
// who they are, and a table has to pick one. Whichever it picked would be
// wrong -- either an authorized developer is told their password is bad, or an
// anonymous peer is handed a live reading of how full the system is.
//
// So the thing that varies travels with the EVENT, not with the identity. A
// renderer projects from an occurrence and never from a reason-only lookup,
// and the witness below is the field it turns on.
//
//	  [Event Raised: Reason, Producer]
//	                 │
//	                 ▼
//	    ┌─────────────────────────┐
//	    │ Occurrence              │
//	    │ • Reason, Producer      │
//	    │ • Kind, Charge          │
//	    │ • Disclosable (Witness) │
//	    │ • Detail (Diagnostic)   │
//	    └────────────┬────────────┘
//	                 │
//	       o.Disclosable == true?
//	       ┌─────────┴─────────┐
//	      YES                 NO
//	       │                   │
//	       ▼                   ▼
//	  [Disclosed Error]   [Generic Refusal]
//	  e.g. SQLSTATE 53300 e.g. SQLSTATE 28000
type Occurrence struct {
	Reason   ReasonID
	Producer ProducerID
	Kind     Kind
	Charge   Charge

	// Disclosable is the AUTHORIZATION WITNESS: proof that this outcome was
	// raised with a verified credential and a checked grant already in hand.
	//
	// It is set by the raise site and carried; it is never derived from the
	// reason. That is what makes the rule survive a reordering -- move a
	// capacity check above the credential check and no witness exists, so the
	// surface stays uniform instead of leaking. A field computed from the
	// reason would leak the moment somebody moved the check, and the code
	// would look correct.
	Disclosable bool

	// Detail is the internal particular -- the refused parameter, the stage
	// that failed. It is for the operator's record and the renderer decides
	// whether any of it reaches the peer, which is usually none of it.
	Detail string
}

// Charges reports whether this occurrence counts against the peer's source
// address.
func (o Occurrence) Charges() bool { return o.Charge.Charges() }

// Occur builds an occurrence, refusing anything the registry has not been told
// about.
//
// NOTHING MAY BE EMITTED THAT WAS NEVER DECLARED, and the check is here rather
// than at the wire because here is where the producer is still known. A reason
// invented at a call site would otherwise arrive at the renderer looking
// exactly like a declared one, carrying whatever charge the renderer defaulted
// to -- which is the shape of the bug this package was written to end.
//
// It also refuses an identity this producer did not declare. That is not
// pedantry: membership is what makes the registry answer "what can happen
// HERE", and a producer emitting another producer's reason makes that question
// unanswerable.
func (r *Registry) Occur(p ProducerID, id ReasonID, opts ...OccurOption) (Occurrence, error) {
	d, ok := r.decls[id]
	if !ok {
		return Occurrence{}, fmt.Errorf("outcome: %s emitted %q, which no producer declared", p, id)
	}
	declared := false
	for _, owner := range r.producers[id] {
		if owner == p {
			declared = true
			break
		}
	}
	if !declared {
		return Occurrence{}, fmt.Errorf("outcome: %s emitted %q, which it did not declare "+
			"(declared by %v)", p, id, producerNames(r.producers[id]))
	}
	o := Occurrence{Reason: id, Producer: p, Kind: d.Kind, Charge: d.Charge}
	for _, opt := range opts {
		opt(&o)
	}
	return o, nil
}

// OccurOption sets what varies per event rather than per identity.
type OccurOption func(*Occurrence)

// Authorized attaches the authorization witness. It is a separate,
// deliberately awkward call rather than a struct field a caller fills in,
// because every use of it should be visible in review: it is the one input
// that decides whether the surface says more than "denied".
func Authorized() OccurOption { return func(o *Occurrence) { o.Disclosable = true } }

// WithDetail records the internal particular.
func WithDetail(detail string) OccurOption {
	return func(o *Occurrence) { o.Detail = detail }
}
