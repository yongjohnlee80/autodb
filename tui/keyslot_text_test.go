package tui

import (
	"strings"
	"testing"
)

// THE MODAL MUST SAY WHICH CLAIM IS WHICH.
//
// The field report: an operator cut a slot from this modal, reopened it, and
// was shown the STARTUP failure again — "auth: no service keyfile" — so they
// concluded the enrolment had silently failed. It had not. The status was a
// boot-time record presented as a present fact, and the cost was that they
// stopped trusting a working mechanism and rebooted the machine to check.
//
// So a verified-after-start install must lead with the verification, date it,
// and report the boot failure as history rather than as the headline.
func TestKeyslotText_EnrolledAfterAFailedBootLeadsWithTheVerification(t *testing.T) {
	got := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: false, Reason: "auth: no service keyfile: /var/lib/autodb-keys/service.key",
		Checked: true, Verified: true, VerifiedAt: "2026-09-08T12:17:00Z",
		SlotPresent: true, SlotPresentKnown: true,
		StoreUnlocked: true,
	})

	if !strings.Contains(got, "ENROLLED AND VERIFIED") {
		t.Errorf("a verified slot is not reported as verified:\n%s", got)
	}
	// THE DEFECT: the startup failure must not be the headline.
	if strings.Contains(got, "UNATTENDED UNLOCK: FAILED") {
		t.Errorf("a verified slot is still headlined as FAILED — this is the stale reading "+
			"that made an operator distrust a working install:\n%s", got)
	}
	// The boot record is still SHOWN, as history and labelled as such: erasing
	// it would be the opposite error.
	if !strings.Contains(got, "At daemon start:") || !strings.Contains(got, "no service keyfile") {
		t.Errorf("the boot failure was erased rather than dated:\n%s", got)
	}
	if !strings.Contains(got, "2026-09-08T12:17:00Z") {
		t.Errorf("the verification is undated, which is the ambiguity this change is about:\n%s", got)
	}
	// And it must not promise the future.
	if !strings.Contains(got, "not a promise about the future") {
		t.Errorf("the text implies a past check guarantees the next restart:\n%s", got)
	}
}

// A DELIBERATE REMOVAL reads as a removal, keeps boot history, and does not
// claim the running process relocked.
func TestKeyslotText_RemovalDoesNotClaimTheProcessRelocked(t *testing.T) {
	got := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: true,
		Checked: true, Verified: false, SlotPresent: false, SlotPresentKnown: true,
		VerifiedAt: "2026-09-08T13:00:00Z",
		Reason:     "", VerifyReason: "the slot was removed",
		StoreUnlocked: true,
	})
	if !strings.Contains(got, "REMOVED") {
		t.Errorf("a removed slot is not reported as removed:\n%s", got)
	}
	if !strings.Contains(got, "still holds the key") {
		t.Errorf("the text does not say work in flight is unaffected:\n%s", got)
	}
	if !strings.Contains(got, "UNLOCKED right now") {
		t.Errorf("a removal is being reported as though the store relocked:\n%s", got)
	}
}

// COMMITTED BUT NOT WORKING is its own state, not folded into success or
// failure — there is a row AND a keyfile to reason about, so the recovery
// differs from "enrolment failed".
func TestKeyslotText_CutButNotWorkingIsItsOwnState(t *testing.T) {
	got := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: false, Reason: "auth: no service keyfile: /k/service.key",
		Checked: true, Verified: false, SlotPresent: true, SlotPresentKnown: true,
		VerifyReason:  "auth: the service keyslot was cut but does not open the store",
		StoreUnlocked: true,
	})
	if !strings.Contains(got, "CUT BUT NOT WORKING") {
		t.Errorf("a committed-but-unverified slot is not named as such:\n%s", got)
	}
	if !strings.Contains(got, "Nothing was re-cut or removed") {
		t.Errorf("the text does not say why it was left alone:\n%s", got)
	}
}

// AND THE ORIGINAL BRANCHES STILL WORK — the boot-only cases, which are what
// an install that has never been touched since start reports. Without this the
// change could have satisfied every cell above by breaking the common path.
func TestKeyslotText_BootOnlyBranchesUnchanged(t *testing.T) {
	active := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: true, Checked: true, Verified: true,
		SlotPresent: true, SlotPresentKnown: true, StoreUnlocked: true,
	})
	if !strings.Contains(active, "UNATTENDED UNLOCK: ACTIVE") {
		t.Errorf("a daemon that unlocked at start no longer reads as ACTIVE:\n%s", active)
	}
	none := keyslotStatusText(KeyslotStatus{Attempted: false, StoreUnlocked: true})
	if !strings.Contains(none, "NOT ENABLED") {
		t.Errorf("an install with no keyslot no longer reads as NOT ENABLED:\n%s", none)
	}
	failed := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: false, Reason: "auth: keyfile mode 0644",
		Checked: true, Verified: false, SlotPresent: true, SlotPresentKnown: true,
		StoreUnlocked: false,
	})
	if !strings.Contains(failed, "UNATTENDED UNLOCK: FAILED") {
		t.Errorf("a boot failure with nothing proven since no longer reads as FAILED:\n%s", failed)
	}
	if !strings.Contains(failed, "57P03") {
		t.Errorf("the failure branch stopped naming the code clients actually see:\n%s", failed)
	}
}

// AN UNANSWERED LOOKUP IS NOT A REMOVAL.
//
// SlotPresent used to come from a query that mapped EVERY failure to false,
// and this file's REMOVED branch reads "attempted, checked, not present" as a
// deliberate deletion. So a database hiccup during the boot probe told an
// operator that somebody had removed their keyslot -- an assertive claim
// manufactured from a question that was never answered. Only ErrNoRows proves
// absence; everything else is ignorance, and ignorance now renders as such.
func TestKeyslotText_UnreadableStoreIsNotReportedAsARemoval(t *testing.T) {
	got := keyslotStatusText(KeyslotStatus{
		Attempted: true, Unlocked: false, Reason: "auth: no service keyfile: /k/service.key",
		Checked: true, Verified: false,
		// The shape a failed inspection produces: not present, and NOT known.
		SlotPresent: false, SlotPresentKnown: false,
		VerifyReason:  "meta: reading the keyslot row: database is locked",
		VerifiedAt:    "2026-09-09T01:00:00Z",
		StoreUnlocked: true,
	})

	if strings.Contains(got, "REMOVED") {
		t.Errorf("a failed inspection is reported as a deliberate removal, which no evidence "+
			"supports:\n%s", got)
	}
	if !strings.Contains(got, "CANNOT BE DETERMINED") {
		t.Errorf("an unreadable store is not reported as unknown:\n%s", got)
	}
	if !strings.Contains(got, "says nothing about whether a slot exists") {
		t.Errorf("the text does not warn that this screen proves nothing either way:\n%s", got)
	}
	if !strings.Contains(got, "database is locked") {
		t.Errorf("the inspection failure itself is not shown:\n%s", got)
	}
}
