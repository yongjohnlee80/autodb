package auth

import (
	"context"
	"os"
	"testing"
)

// A SUCCESSFUL ENROLMENT MUST STOP THE STATUS REPORTING THE BOOT FAILURE —
// without erasing what the boot probe actually found.
//
// This is the field report: an operator cut a slot from the TUI, reopened the
// modal, and was shown "auth: no service keyfile" again. The enrolment had
// worked (the keyfile was on disk, 0600, owned by the service account, and the
// next boot unlocked from it). What was stale was the READING: keyslot.status
// returned the record UnlockWithServiceKeyslot writes once at startup, and
// enrolment never refreshed anything. They read it as a silent failure and
// rebooted the machine to find out.
//
// So the two claims are now two records, and this asserts BOTH halves: the
// current one moves, the boot one does not.
func TestKeyslot_EnrolmentRefreshesNowAndLeavesBootHistoryAlone(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	// The boot probe, with no slot cut yet: this is the failure the operator
	// kept being shown.
	if err := s.UnlockWithServiceKeyslot(ctx); err == nil {
		t.Fatal("the boot probe succeeded with no slot enrolled")
	}
	boot := s.ServiceKeyslotStatus()
	if boot.Unlocked || boot.Reason == "" {
		t.Fatalf("the boot record did not capture a failure: %+v", boot)
	}
	bootReason := boot.Reason

	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	if _, err := os.Stat(keyfile); err != nil {
		t.Fatalf("enrolment reported success without a keyfile: %v", err)
	}

	// THE FIX: the current record says the slot was proven, and when.
	now := s.ServiceKeyslotNow()
	if !now.Checked || !now.Verified {
		t.Errorf("a successful enrolment did not record a verified slot: %+v", now)
	}
	if now.At.IsZero() {
		t.Error("the verification has no timestamp, so the modal cannot say WHEN it was proven")
	}
	if !now.SlotPresent {
		t.Error("a slot was cut and the current record says none is present")
	}
	if now.Reason != "" {
		t.Errorf("a verified slot carries a failure reason: %q", now.Reason)
	}

	// AND THE OTHER HALF, which the first version of this fix would have
	// broken: verifying by calling UnlockWithServiceKeyslot would have
	// overwritten the very history it was meant to preserve.
	if after := s.ServiceKeyslotStatus(); after.Reason != bootReason || after.Unlocked {
		t.Errorf("enrolment rewrote the boot record: was %q/unlocked=%v, now %q/unlocked=%v",
			bootReason, false, after.Reason, after.Unlocked)
	}
}

// REMOVAL INVALIDATES THE CURRENT CLAIM, keeps boot history, and does NOT
// claim the running process relocked — it still holds the key it unwrapped.
func TestKeyslot_RemovalInvalidatesNowWithoutRewritingBoot(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	_ = s.UnlockWithServiceKeyslot(ctx)
	bootBefore := s.ServiceKeyslotStatus()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	// POSITIVE CONTROL: verified before the removal, so the change below is
	// caused by the removal and not by never having been verified.
	if !s.ServiceKeyslotNow().Verified {
		t.Fatal("not verified before removal; the assertion below would prove nothing")
	}

	if err := s.RemoveServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("RemoveServiceKeyslot: %v", err)
	}
	now := s.ServiceKeyslotNow()
	if now.Verified {
		t.Error("the current claim still says verified after the slot was removed")
	}
	if now.SlotPresent {
		t.Error("the current claim still says a slot is present after removal")
	}
	if now.Reason == "" {
		t.Error("the removal left no reason, so the modal cannot say why unattended unlock stopped")
	}
	if after := s.ServiceKeyslotStatus(); after.Reason != bootBefore.Reason ||
		after.Unlocked != bootBefore.Unlocked || after.Attempted != bootBefore.Attempted {
		t.Errorf("removal rewrote boot history: %+v -> %+v", bootBefore, after)
	}
	// The store this process holds open is NOT relocked by a removal.
	if !s.Unlocked() {
		t.Error("removing the slot relocked the running process; it holds the key it already unwrapped")
	}
}

// A LATE VERIFICATION MUST NOT RESURRECT A REMOVED SLOT.
//
// Verification is slower than the mutation that triggers it, so without a
// generation check a verify that started before a removal could land after it
// and write "verified" for a slot that no longer exists — the operator would
// then be told unattended unlock is fine while the next restart sits locked.
func TestKeyslot_LateVerificationIsDiscardedAfterRemoval(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	// A verification that started BEFORE the removal carries the older
	// generation.
	stale := s.ServiceKeyslotNow()
	staleGen := s.keyslotGen

	if err := s.RemoveServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("RemoveServiceKeyslot: %v", err)
	}
	// Now it lands, claiming success.
	if kept := s.setKeyslotNow(staleGen, ServiceKeyslotCurrent{
		Checked: true, Verified: true, At: s.now(), SlotPresent: true,
	}); kept {
		t.Error("a verification from before the removal was accepted")
	}
	if s.ServiceKeyslotNow().Verified {
		t.Error("the stale verification overwrote the post-removal state")
	}
	_ = stale
}

// AND A CURRENT-GENERATION VERIFICATION IS KEPT — the positive control for the
// generation check, without which a setter that rejected everything would
// satisfy the cell above.
func TestKeyslot_CurrentGenerationVerificationIsKept(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	ctx := context.Background()
	_ = ctx

	gen := s.bumpKeyslotGen()
	if kept := s.setKeyslotNow(gen, ServiceKeyslotCurrent{
		Checked: true, Verified: true, At: s.now(), SlotPresent: true,
	}); !kept {
		t.Fatal("a verification under the current generation was discarded")
	}
	if !s.ServiceKeyslotNow().Verified {
		t.Error("the accepted verification was not stored")
	}
}

// A BROKEN PAIR RECORDS A FAILURE IN THE CURRENT RECORD ONLY.
//
// This is the same code path enrolment uses to prove itself, driven against a
// pair that has been separated after the fact — the ordinary way this breaks
// in the field (a keyfile deleted, restored from a backup, or re-moded).
func TestKeyslot_VerificationFailureTouchesOnlyTheCurrentRecord(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	_ = s.UnlockWithServiceKeyslot(ctx)
	boot := s.ServiceKeyslotStatus()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	if err := os.Remove(keyfile); err != nil {
		t.Fatal(err)
	}

	gen := s.bumpKeyslotGen()
	if err := s.verifySlotOpens(ctx, gen); err == nil {
		t.Fatal("verification succeeded with the keyfile deleted")
	}
	now := s.ServiceKeyslotNow()
	if now.Verified || now.Reason == "" {
		t.Errorf("the failure was not recorded as such: %+v", now)
	}
	if after := s.ServiceKeyslotStatus(); after != boot {
		t.Errorf("a verification failure rewrote boot history: %+v -> %+v", boot, after)
	}
}
