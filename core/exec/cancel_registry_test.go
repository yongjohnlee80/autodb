package exec

import (
	"context"
	"testing"
	"time"
)

// THE CANCEL REGISTRY (matrix row 2.3, F3a).
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
