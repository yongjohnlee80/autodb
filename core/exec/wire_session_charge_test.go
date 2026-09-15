package exec

import "testing"

// Every declared denial reason must carry a ruled charge class.
//
// This is the test that makes the registry a registry rather than a map with
// gaps. The defect it exists to prevent is the one ADR 0180 was written for:
// a reason nobody classified being charged to the credential throttle by
// default, silently, until a developer is banned for running out of capacity.
func TestEveryDenialReasonHasAChargeClass(t *testing.T) {
	for _, reason := range DenialReasons() {
		if _, ok := DenialCharge(reason); !ok {
			t.Errorf("denial reason %q has no ruled charge class — classify it in denialCharge", reason)
		}
	}
}

// The eight reasons ADR 0180 §3.2 named as mis-charged must not charge.
func TestTheMischargedReasonsNoLongerCharge(t *testing.T) {
	for _, tc := range []struct {
		reason string
		class  ChargeClass
	}{
		{DenyLeaseCap, ChargeCapacity},
		{DenySessionCap, ChargeCapacity},
		{DenyResidentBudget, ChargeCapacity},
		{DenyProfileRefuses, ChargeNone},
		{DenyLeaseEncoding, ChargeNone},
		{DenyNoSuchDatabase, ChargeNone},
		{DenyNoGrant, ChargeNone},
		{DenyPATUnscoped, ChargeNone},
		{DenyStartupGUC, ChargeNone},
	} {
		got, ok := DenialCharge(tc.reason)
		if !ok {
			t.Fatalf("%s: unclassified", tc.reason)
		}
		if got != tc.class {
			t.Errorf("%s: class %v, want %v", tc.reason, got, tc.class)
		}
		if got.Charges() {
			t.Errorf("%s must NOT charge the credential throttle — this is the lockout defect", tc.reason)
		}
	}
}

// Credential and protocol strictness is explicitly unchanged (D6).
func TestCredentialReasonsStillCharge(t *testing.T) {
	for _, reason := range []string{
		DenyBadCredential, DenyUserMismatch, DenyIPNotAdmitted, DenyPATIPNarrowed,
	} {
		class, ok := DenialCharge(reason)
		if !ok {
			t.Fatalf("%s: unclassified", reason)
		}
		if !class.Charges() {
			t.Errorf("%s must still charge: bad credentials keep today's strictness", reason)
		}
	}
}
