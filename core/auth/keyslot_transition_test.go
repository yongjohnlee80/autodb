package auth

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// THE PUBLISHED STATE MUST AGREE WITH THE ROW, whatever order the two
// mutations arrive in.
//
// The generation counter alone did not guarantee that: the enrol took its
// generation AFTER its commit, so a removal could commit, publish "removed",
// and then be overwritten by the enrol's newer generation — leaving the modal
// reporting a verified unattended unlock for a slot that no longer existed.
// Ordering a counter around one of the two steps cannot fix an interleaving of
// both, so the whole transition is serialized.
//
// The invariant asserted here is the one an operator relies on: never
// "verified" while the row is gone.
func TestKeyslot_StateNeverClaimsAVerifiedSlotThatIsGone(t *testing.T) {
	s, _, _, _ := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	if err := s.RemoveServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("RemoveServiceKeyslot: %v", err)
	}

	// Drive a removal into the post-commit window. A plain race cannot hit an
	// interval this short reliably, and a cell that only SOMETIMES observes
	// the defect is a cell that reports the fix as working.
	var wg sync.WaitGroup
	var removeErr error
	s.hookAfterKeyslotCommit = func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			removeErr = s.RemoveServiceKeyslot(ctx, rootTok, testIP)
		}()
		// Give the removal a chance to reach the mutation. With the
		// transition serialized it blocks here and lands after this
		// enrolment; unserialized it proceeds and interleaves.
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("the second EnrollServiceKeyslot failed: %v", err)
	}
	s.hookAfterKeyslotCommit = nil
	wg.Wait()
	if removeErr != nil {
		t.Fatalf("the concurrent removal failed, so the window was never exercised: %v", removeErr)
	}

	// THE INVARIANT an operator relies on: never "verified" while the row is
	// gone. The removal ran last, so the row is gone and the state must say so.
	rowThere, known := s.serviceSlotPresence(ctx)
	if !known {
		t.Fatal("could not read the slot row, so this cell cannot judge the outcome")
	}
	now := s.ServiceKeyslotNow()
	if rowThere {
		t.Fatalf("the removal did not land last, so this cell is not testing the "+
			"interleaving it exists for (state %+v)", now)
	}
	if now.Verified {
		t.Errorf("the state claims a VERIFIED slot while the row is gone -- an enrolment "+
			"published over a removal that had already happened: %+v", now)
	}
	if now.SlotPresent {
		t.Errorf("the state claims a slot is present while the row is gone: %+v", now)
	}
}

// A FAILING UNLINK MUST NOT LEAVE A STALE "IT WORKS" BEHIND.
//
// The removal used to return the cleanup error BEFORE publishing state, so
// after the authoritative row was deleted the current claim still read
// verified and slot-present. The row is what decides; the truth goes out as
// soon as the row does, and the leftover keyfile is reported afterwards.
func TestKeyslot_RemovalPublishesTruthEvenWhenTheKeyfileCannotBeDeleted(t *testing.T) {
	s, _, _, keyfile := newKeyslotFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unlink cannot be refused by directory permissions, so " +
			"this cell cannot induce the failure it is about")
	}
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	if !s.ServiceKeyslotNow().Verified {
		t.Fatal("not verified before the removal; the assertion below would prove nothing")
	}

	// Refuse the unlink by taking write permission off the directory.
	dir := filepath.Dir(keyfile)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, info.Mode()) })

	err = s.RemoveServiceKeyslot(ctx, rootTok, testIP)
	if err == nil {
		t.Fatal("the unlink did not fail, so this cell observed nothing")
	}
	// The error still reaches the operator...
	if !containsAll(err.Error(), "keyfile remains", keyfile) {
		t.Errorf("the cleanup failure does not name the file left behind: %v", err)
	}
	// ...AND the state is truthful about the row that is already gone.
	now := s.ServiceKeyslotNow()
	if now.Verified || now.SlotPresent {
		t.Errorf("after the row was deleted the state still claims a working slot: %+v", now)
	}
	if present, known := s.serviceSlotPresence(ctx); known && present {
		t.Error("the row survived a removal that reported only a cleanup failure")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SLOT PRESENCE IS ASKED OF THE ROW, not inferred from the error.
//
// Deriving it from the error class got the common field failure exactly
// backwards: a deleted KEYFILE leaves the row perfectly present, and reporting
// "no slot" sends an operator to re-enroll — which is then refused, because
// the row is there.
func TestKeyslot_MissingKeyfileStillReportsTheSlotAsPresent(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	if err := s.EnrollServiceKeyslot(ctx, rootTok, testIP); err != nil {
		t.Fatalf("EnrollServiceKeyslot: %v", err)
	}
	if err := os.Remove(keyfile); err != nil {
		t.Fatal(err)
	}

	gen := s.bumpKeyslotGen()
	if err := s.verifySlotOpens(ctx, gen); err == nil {
		t.Fatal("verification succeeded with no keyfile")
	}
	now := s.ServiceKeyslotNow()
	if now.Verified {
		t.Error("a missing keyfile verified")
	}
	if !now.SlotPresent {
		t.Error("a missing keyfile was reported as a missing SLOT; the row is still there, " +
			"and re-enrolling would be refused")
	}
}
