package outcome

import (
	"strings"
	"testing"
)

const (
	engine = ProducerID("engine")
	door   = ProducerID("front-door")
	stmt   = ProducerID("statement-admission")
)

func capacity(id string) Decl { return Decl{ID: ReasonID(id), Kind: Refusal, Charge: Capacity} }

// ONE IDENTITY, SEVERAL PRODUCERS, and this is the normal case rather than an
// error. The engine RAISES a capacity refusal and the front door RENDERS it;
// an earlier design put a single phase on the reason, which forces one of them
// to lie about who it belongs to.
func TestCompose_AnIdentityMayHaveSeveralProducers(t *testing.T) {
	t.Parallel()
	r, err := Compose(
		Registration{Producer: engine, Outcomes: []Decl{capacity("frontdoor/lease-cap-exceeded")}},
		Registration{Producer: door, Outcomes: []Decl{capacity("frontdoor/lease-cap-exceeded")}},
	)
	if err != nil {
		t.Fatalf("two producers sharing one identity was refused: %v", err)
	}
	got := r.Producers("frontdoor/lease-cap-exceeded")
	if len(got) != 2 || got[0] != engine || got[1] != door {
		t.Errorf("producers = %v, want both, in registration order", got)
	}
	if d, ok := r.Lookup("frontdoor/lease-cap-exceeded"); !ok || d.Charge != Capacity {
		t.Errorf("lookup = %+v %t, want one agreed declaration", d, ok)
	}
}

// AGREEING IS THE CONDITION OF SHARING. Two producers may declare one
// identity; they may not disagree about what it is, because then the charge
// would depend on which of them registered last.
func TestCompose_ConflictingMetadataIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Compose(
		Registration{Producer: engine, Outcomes: []Decl{capacity("x")}},
		Registration{Producer: door, Outcomes: []Decl{{ID: "x", Kind: Refusal, Charge: Credential}}},
	)
	if err == nil {
		t.Fatal("one identity was accepted as capacity from one producer and credential from " +
			"another; whichever registered last would decide whether a developer gets banned")
	}
	for _, want := range []string{"capacity", "credential", "engine", "front-door"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so a reader cannot find either side: %v", want, err)
		}
	}
}

// A PRODUCER MUST NOT DISAGREE WITH ITSELF EITHER.
func TestCompose_ADuplicatePairIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Compose(Registration{Producer: engine, Outcomes: []Decl{capacity("x"), capacity("x")}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a producer declaring one identity twice was accepted: %v", err)
	}
}

// THE ZERO VALUES FAIL CLOSED. The failure mode of a forgotten charge is that
// something gets charged by accident, and that is the failure that banned a
// developer for running out of capacity -- so "not stated" must be an error
// and never a default.
func TestCompose_UnsetKindOrChargeIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		decl Decl
		want string
	}{
		{"no kind", Decl{ID: "x", Charge: None}, "no kind"},
		{"no charge", Decl{ID: "x", Kind: Refusal}, "no charge"},
		{"no identity", Decl{Kind: Refusal, Charge: None}, "no identity"},
	} {
		if _, err := Compose(Registration{Producer: engine, Outcomes: []Decl{c.decl}}); err == nil {
			t.Errorf("%s: accepted, so the zero value became a silent default", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to name the missing field", c.name, err)
		}
	}
	if _, err := Compose(Registration{Outcomes: []Decl{capacity("x")}}); err == nil {
		t.Error("a registration with no producer was accepted; membership is the producer's, " +
			"so an anonymous one declares nothing it can be held to")
	}
}

// NOTHING MAY BE EMITTED THAT WAS NEVER DECLARED. A reason invented at a call
// site would otherwise reach the renderer looking exactly like a declared one,
// carrying whatever charge the renderer defaulted to.
func TestOccur_AnUndeclaredIdentityIsRefused(t *testing.T) {
	t.Parallel()
	r, err := Compose(Registration{Producer: engine, Outcomes: []Decl{capacity("known")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Occur(engine, "invented-at-the-call-site"); err == nil {
		t.Fatal("an undeclared identity was emitted")
	}
	// And a producer may not emit another producer's identity: membership is
	// what makes "what can happen here" an answerable question.
	if _, err := r.Occur(door, "known"); err == nil {
		t.Fatal("a producer emitted an identity it never declared")
	}
	if _, err := r.Occur(engine, "known"); err != nil {
		t.Fatalf("the declared case was refused: %v", err)
	}
}

// THE WITNESS TRAVELS WITH THE EVENT, never with the identity.
//
// This is the whole reason the registry does not render. The same identity is
// 28000 to a stranger and 53300 to a caller who has proved who they are; a
// table from reason to code has to pick one and is wrong for the other.
func TestOccur_TheWitnessIsPerEventAndNotPerIdentity(t *testing.T) {
	t.Parallel()
	r, err := Compose(Registration{Producer: engine, Outcomes: []Decl{capacity("cap")}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := r.Occur(engine, "cap")
	if err != nil {
		t.Fatal(err)
	}
	witnessed, err := r.Occur(engine, "cap", Authorized(), WithDetail("lease"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Disclosable {
		t.Error("an occurrence carries the witness without being given one; it is being " +
			"derived from the reason, which is what leaks the moment somebody reorders a check")
	}
	if !witnessed.Disclosable {
		t.Error("the witness did not survive onto the occurrence")
	}
	if plain.Reason != witnessed.Reason || plain.Charge != witnessed.Charge {
		t.Error("the identity changed with the witness; they are the same outcome seen by " +
			"callers with different standing")
	}
	if witnessed.Detail != "lease" {
		t.Errorf("detail = %q", witnessed.Detail)
	}
}

// THE CHARGE QUESTION, answered the way the ruling answers it.
func TestCharge_OnlyCredentialAndProtocolCount(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		charge Charge
		want   bool
		why    string
	}{
		{Credential, true, "the peer presented something wrong"},
		{Protocol, true, "otherwise an attacker switches to handshake grinding for a fresh allowance"},
		{Capacity, false, "WE ran out; charging it is what banned a developer"},
		{None, false, "our configuration or our bug, after a verified credential"},
		{NotApplicable, false, "there is no per-source counter in reach at all"},
		{ChargeUnset, false, "an unstated charge must never be a charging one"},
	} {
		if got := c.charge.Charges(); got != c.want {
			t.Errorf("%s.Charges() = %t, want %t — %s", c.charge, got, c.want, c.why)
		}
	}
	// None and NotApplicable are DIFFERENT ANSWERS to different questions, and
	// collapsing them loses the distinction between "we decided not to charge"
	// and "the question does not arise here".
	if None == NotApplicable {
		t.Error("None and NotApplicable are the same value")
	}
}

// A walk over the registry has to be stable, or every assertion built on one
// is a coin toss.
func TestReasons_AreStablyOrdered(t *testing.T) {
	t.Parallel()
	r, err := Compose(
		Registration{Producer: stmt, Outcomes: []Decl{
			{ID: "c", Kind: Refusal, Charge: NotApplicable},
			{ID: "a", Kind: Refusal, Charge: NotApplicable},
			{ID: "b", Kind: Note, Charge: NotApplicable},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		got := r.Reasons()
		if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Fatalf("Reasons() = %v on run %d, want a stable sorted order", got, i)
		}
	}
}
