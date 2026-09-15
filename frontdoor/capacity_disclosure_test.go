package frontdoor

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// A fully authorized caller refused for capacity is TOLD it was capacity.
//
// This is the developer-visible half of the 2026-09-15 lockout. The charge fix
// stopped the ban; this stops the misdirection. A pool at its limit rendered
// the uniform "authentication failed", so a developer whose credentials were
// perfect went hunting a password problem that did not exist.
func TestCapacityDisclosure_AnAuthorizedCallerIsToldItIsCapacity(t *testing.T) {
	frame := denialFor(denialReason(exec.DenyLeaseCap), true)

	if frame.Code != CapacitySQLState {
		t.Errorf("code = %q, want %q — a code every client already renders sensibly",
			frame.Code, CapacitySQLState)
	}
	if frame.Code == DenialSQLState {
		t.Error("an authorized capacity refusal still reads as a credential failure")
	}
	if frame.Message != CapacityMessage {
		t.Errorf("message = %q, want the fixed capacity message", frame.Message)
	}
	// FIXED MESSAGE, NO CAUSE. Which cap was reached is the operator's
	// business and stays in the audit trail.
	for _, cap := range []string{exec.DenyLeaseCap, exec.DenySessionCap, exec.DenyResidentBudget} {
		f := denialFor(denialReason(cap), true)
		if f.Message != CapacityMessage {
			t.Errorf("%s: message varies with the cause, which tells a caller which "+
				"limit they hit", cap)
		}
		if f.Detail == cap {
			t.Errorf("%s: the internal reason reached the wire", cap)
		}
	}
}

// WITHOUT THE WITNESS, NOTHING IS DISCLOSED. A stranger learns only that the
// connection was refused, which is all a stranger learns about anything.
func TestCapacityDisclosure_AStrangerLearnsNothing(t *testing.T) {
	for _, reason := range []string{
		exec.DenyLeaseCap, exec.DenySessionCap, exec.DenyResidentBudget,
		exec.DenyBadCredential, exec.DenyNoGrant,
	} {
		frame := denialFor(denialReason(reason), false)
		if frame.Code != DenialSQLState {
			t.Errorf("%s: code = %q without a witness, want the uniform %q — a capacity "+
				"oracle for an unauthenticated peer is exactly what the uniform denial "+
				"exists to prevent", reason, frame.Code, DenialSQLState)
		}
		if frame.Message != DenialMessage {
			t.Errorf("%s: message differs from the uniform one", reason)
		}
	}
}

// THE WITNESS COMES FROM THE ENGINE, and only from a refusal raised with a
// verified credential in hand.
//
// This is what makes the exemption survive a reordering: a capacity check
// moved above the credential check produces no witness, so the wire stays
// uniform instead of leaking.
func TestCapacityDisclosure_TheWitnessIsNotDerivedFromTheReason(t *testing.T) {
	// A denial built the ordinary way -- as a pre-authorization refusal would
	// be -- carries no witness, EVEN FOR A CAPACITY REASON.
	plain := exec.WireDenial(exec.DenyLeaseCap)
	if exec.DenialDisclosable(plain) {
		t.Error("a denial built without the authorized constructor claims disclosure; " +
			"the witness must be earned, not inferred from the reason")
	}
	if frame := denialFor(denialReason(exec.DenyLeaseCap), exec.DenialDisclosable(plain)); frame.Code != DenialSQLState {
		t.Errorf("code = %q for an unwitnessed capacity reason, want the uniform %q",
			frame.Code, DenialSQLState)
	}
}
