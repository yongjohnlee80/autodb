package exec

import (
	"context"
	"errors"
	"testing"
	"time"
)

// THE CANCEL REGISTRY (F3a — the ENGINE half of the CancelRequest behaviour).
//
// Deliberately NOT written as a matrix-row citation. These cells prove the
// registry honours a key; they do not prove the matrix's CancelRequest row, which requires the
// plaintext CancelRequest to be processed per §6.4 — issuance at open,
// revocation at close, statement-only cancellation, stale audit, race
// boundaries — none of which is reachable until the listener half exists.
// Naming that row here would promote it in the coverage gate on the strength of
// a unit that never touches the wire (lector, PR #41 r0).
//
// The pair a client receives in BackendKeyData is a CAPABILITY: whoever holds
// it stops that session's statement without presenting a credential, because
// the cancelling connection has no TLS and no startup and cannot present one.
// So the secret is the whole of the authority, and every cell below is about
// what that implies.

func issuedKey(t *testing.T, f *fixture, id SessionID, userID int64) CancelKey {
	t.Helper()
	k, err := f.eng.IssueCancelKey(id, userID)
	if err != nil {
		t.Fatalf("IssueCancelKey: %v", err)
	}
	return k
}

// A KEY CANCELS ITS OWN SESSION'S STATEMENT — and leaves the session open.
//
// The second half is the security half. A cancel that closed the connection
// would let anyone holding a guessed pair disconnect a user, which is a denial
// of service wearing a feature's clothes.
func TestCancelRegistry_CancelsTheStatementAndKeepsTheSession(t *testing.T) {
	t.Parallel()
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	uid := userIDOf(t, f)
	key := issuedKey(t, f, sid, uid)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "SELECT pg_sleep(30)", testIP)
		done <- err
	}()
	<-started
	// Wait until the statement is genuinely in flight, or the cancel would
	// arrive before there is anything to cancel and the cell would pass on
	// an empty registry.
	waitForInFlight(t, f, sid, uid)

	if !f.eng.CancelByKey(ctx, key) {
		t.Fatal("the key did not match its own session")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the statement completed; nothing was cancelled")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the statement was not cancelled: a client pressing Ctrl-C gets a closed " +
			"socket and a query that runs on, which is worse than offering no cancel at all " +
			"because the client believes it worked")
	}

	if _, err := f.eng.sessions.lookup(sid, uid); err != nil {
		t.Error("the session was closed by a cancel. Anyone holding a guessed pair could then " +
			"disconnect a user, which is a denial of service dressed as a feature")
	}
}

func waitForInFlight(t *testing.T, f *fixture, sid SessionID, uid int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := f.eng.sessions.lookup(sid, uid)
		if err == nil {
			s.mu.Lock()
			running := s.runCancel != nil
			s.mu.Unlock()
			if running {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no statement ever went in flight; the cancel would have nothing to act on")
}

// AN UNKNOWN PAIR IS A SILENT NO-OP, and costs the same as a wrong secret.
//
// Returning early on an unknown process id would let a caller learn which ids
// are live by timing two guesses — the registry walked from outside, one
// comparison at a time, by someone who never authenticated.
func TestCancelRegistry_AMissCostsWhatAWrongSecretCosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	key := issuedKey(t, f, "sess-real", 1)

	unknown := CancelKey{ProcessID: key.ProcessID ^ 0x5A5A5A5A}
	before := f.eng.CancelCompareCount()
	if f.eng.CancelByKey(ctx, unknown) {
		t.Fatal("an unknown process id was accepted")
	}
	afterUnknown := f.eng.CancelCompareCount()

	wrongSecret := CancelKey{ProcessID: key.ProcessID}
	wrongSecret.Secret[0] = key.Secret[0] ^ 0xFF
	if f.eng.CancelByKey(ctx, wrongSecret) {
		t.Fatal("a wrong secret was accepted")
	}
	afterWrong := f.eng.CancelCompareCount()

	if afterUnknown-before != 1 || afterWrong-afterUnknown != 1 {
		t.Errorf("an unknown id did %d comparisons and a wrong secret %d; they must cost the "+
			"same, or a caller learns which process ids are live by timing two guesses",
			afterUnknown-before, afterWrong-afterUnknown)
	}
}

// THE SECRET IS THE AUTHORITY, on a LIVE session.
//
// A live one, because CancelByKey ends with a session lookup: against an
// invented id it returns false whatever the secret did, so a cell built that
// way cannot see the comparison at all. The first version was built that way,
// and a mutation that never compared the secret sailed through it — the third
// assertion in this program to pass for a reason other than the one it named.
//
// The positive branch is what makes the negatives mean anything: the right
// secret MUST match, or "wrong secret refused" is satisfied by refusing
// everything.
func TestCancelRegistry_TheSecretIsTheAuthority(t *testing.T) {
	t.Parallel()
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	uid := userIDOf(t, f)
	key := issuedKey(t, f, sid, uid)

	if !f.eng.CancelByKey(ctx, key) {
		t.Fatal("the right pair did not match its own live session, so every refusal below " +
			"would be a registry that refuses everything")
	}

	wrongSecret := CancelKey{ProcessID: key.ProcessID}
	copy(wrongSecret.Secret[:], key.Secret[:])
	wrongSecret.Secret[0] ^= 0xFF
	if f.eng.CancelByKey(ctx, wrongSecret) {
		t.Fatal("the right process id with the WRONG secret was accepted. The cancelling " +
			"connection cannot authenticate, so the secret is the whole of the authority — " +
			"and a process id is not a secret: it is four bytes a client already has")
	}

	wrongPID := CancelKey{ProcessID: key.ProcessID ^ 0x5A5A5A5A, Secret: key.Secret}
	if f.eng.CancelByKey(ctx, wrongPID) {
		t.Fatal("a secret matched a process id it was not issued against")
	}
}

// A REVOKED KEY STOPS WORKING.
//
// A key outliving its session is a capability pointing at nothing — and worse,
// at whatever later takes the same process id.
func TestCancelRegistry_RevocationEndsTheCapability(t *testing.T) {
	t.Parallel()
	// A REAL session, and this is the whole point of the cell.
	//
	// The first version used an invented session id, so CancelByKey returned
	// false because the SESSION lookup missed — not because the key had been
	// revoked. Making revocation a no-op left it green: it was asserting that
	// a key for a session that never existed does not work, which nothing
	// could have made false. A cell that cannot distinguish its own subject
	// from a missing fixture is not testing the subject.
	f, _, sid, _ := pgSession(t)
	ctx := context.Background()
	uid := userIDOf(t, f)
	key := issuedKey(t, f, sid, uid)

	if !f.eng.CancelByKey(ctx, key) {
		t.Fatal("the freshly issued key did not match its own live session, so nothing below " +
			"could tell revocation from a broken key")
	}

	f.eng.RevokeCancelKey(sid)
	if f.eng.CancelByKey(ctx, key) {
		t.Fatal("a revoked key still matched a live session. The pair outlives the session it " +
			"was minted for, so a client holding it can act on whatever later takes that " +
			"process id")
	}
}

// THE REGISTERED PAIR IS THE PAIR GIVEN, OR THE CALLER IS TOLD (PR #44 r0).
//
// RegisterCancelKey receives the pair the front door has ALREADY composed
// into a BackendKeyData frame. A silent redraw on collision would record a
// process id the client was never sent — an unhonourable key handed out on
// exactly the collision path. The defect was real: the first version
// redrew locally and returned nil while the wire frame carried the original
// pid (lector, PR #44 r0 P1).
//
// This is the ENGINE cell; the front door's remint-and-retry half is in
// frontdoor/cancel_test.go. Neither can stand in for the other: this one
// proves the typed refusal, that one proves the retry composes one pair.
func TestCancelRegistry_CollisionIsRefusedNotRedrawn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	first := issuedKey(t, f, "sess-first", 1)
	// A second session presenting the SAME process id must be refused with
	// the typed collision error — never silently registered elsewhere.
	second := CancelKey{ProcessID: first.ProcessID}
	copy(second.Secret[:], first.Secret[:])
	second.Secret[0] ^= 0xFF // a different pair, so only the pid collides
	if err := f.eng.RegisterCancelKey("sess-second", 2, second); !errors.Is(err, ErrCancelKeyCollision) {
		t.Fatalf("a colliding registration returned %v, want ErrCancelKeyCollision — the "+
			"caller must be told, because only it knows the frame the client will receive", err)
	}

	// The refusal changed NOTHING, read from the registry itself: CancelByKey
	// resolves through a live session, and these ids are deliberately not
	// live ones, so the lookup would answer false whatever the map held —
	// the fixture trap the file comment above warns about.
	f.eng.cancels.mu.Lock()
	held, present := f.eng.cancels.by[first.ProcessID]
	secondPids := []uint32{}
	for pid, target := range f.eng.cancels.by {
		if target.id == "sess-second" {
			secondPids = append(secondPids, pid)
		}
	}
	f.eng.cancels.mu.Unlock()
	if !present || held.id != "sess-first" || held.secret != first.Secret {
		t.Fatalf("the collision refusal disturbed the existing entry: %+v — a refused "+
			"registration must leave the registry exactly as it was", held)
	}
	if len(secondPids) != 0 {
		t.Fatalf("the refused session holds %d registered pid(s) %v — the colliding "+
			"registration was stored after all, possibly under a redrawn pid", len(secondPids), secondPids)
	}
}

// REREGISTRATION FOR THE SAME SESSION REPLACES IN PLACE, and cannot disarm
// the session: the old entry survives until the new one is placed.
func TestCancelRegistry_ReregistrationReplacesWithoutDisarming(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	old := issuedKey(t, f, "sess-re", 1)
	// Reregister the SAME session under a fresh pair, colliding with nobody.
	next := CancelKey{ProcessID: old.ProcessID ^ 0x1}
	copy(next.Secret[:], old.Secret[:])
	next.Secret[1] ^= 0xFF
	if err := f.eng.RegisterCancelKey("sess-re", 1, next); err != nil {
		t.Fatalf("reregistration: %v", err)
	}

	// Both halves read from the registry itself: CancelByKey resolves
	// through a live session and "sess-re" is deliberately not one.
	f.eng.cancels.mu.Lock()
	newEntry, newHeld := f.eng.cancels.by[next.ProcessID]
	oldEntry, oldHeld := f.eng.cancels.by[old.ProcessID]
	count := 0
	for _, target := range f.eng.cancels.by {
		if target.id == "sess-re" {
			count++
		}
	}
	f.eng.cancels.mu.Unlock()
	if !newHeld || newEntry.id != "sess-re" || newEntry.secret != next.Secret {
		t.Fatal("the replacement pair was not registered")
	}
	if count != 1 {
		t.Fatalf("the session holds %d keys, want exactly 1 — a session whose open was "+
			"retried would carry two live capabilities", count)
	}
	// The pid is only "old" when the replacement genuinely moved; if the
	// reregistration kept it (it must not — next.ProcessID differs), this
	// would double-count. It differs by construction, so the old pid must
	// be free of this session.
	if oldHeld && oldEntry.id == "sess-re" {
		t.Fatal("the replaced pair still points at the session")
	}
}
