package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// A GENUINELY LOCKED STORE produces ErrLocked AT THE WIRE SEAM, un-wrapped.
//
// THE LINK THE WHOLE OF ADR-0087 A1.3 RESTS ON, and until this cell it was the
// one claim in the chain nobody had measured. A reviewer caught it and was
// direct that the gap was his: he established the link in the DESIGN round by
// reading openTarget -> DecryptSecret -> ErrLocked, said at the time it was a
// source trace rather than a measurement, then asked for three cells and did
// not ask for the one covering the claim he had only traced.
//
// The frontdoor cells drive fakeAuth{err: auth.ErrLocked}. They prove the
// MAPPING — given ErrLocked at this seam the wire answers 57P03, identically
// for four callers, uncharged. They cannot prove the ARRIVAL, because they
// inject the very thing in question. And the RPC path's locked cell
// (engine_test.go, TestEngine_LockedBeforeLogin) drives Execute, which is a
// DIFFERENT path from the one A1.3 is about.
//
// WHY THE SECOND ASSERTION IS THE LOAD-BEARING ONE. frontdoor/auth.go checks
//
//	if reason := exec.DenialReason(aerr); reason != "" { ... }   // line 240
//	if errors.Is(aerr, auth.ErrLocked) { ... }                   // line 253
//
// in that order. If OpenWireSessionWith ever converts the error out of
// openTarget into a wireDenial — exactly the tidying somebody does when
// unifying error handling in that function — a locked store answers 28000
// again, SILENTLY, with every existing cell green: the injection cells inject
// past the change and the RPC cell exercises another path. So this asserts not
// only that the error IS ErrLocked but that it carries NO denial reason.
func TestWireOpen_LockedStoreSurfacesAtTheFirstStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _, secret, dbName := wireFixture(t)

	// CONTROL FIRST: the session opens while the store is UNLOCKED, so every
	// refusal below is about the lock rather than about this fixture never
	// having worked.
	res, err := f.eng.OpenWireSessionWith(ctx, WireOpen{
		PAT: secret, StartupUser: "root", Database: dbName, IP: testIP,
	})
	if err != nil {
		t.Fatalf("control: the wire session did not open on an UNLOCKED store (%v); "+
			"nothing below would be observable", err)
	}
	f.eng.CloseWireSession(ctx, res.SessionID, res.UserID, testIP, "cell done")

	// A FRESH PROCESS: same store, same rows, no passphrase login — which is
	// exactly a daemon that rebooted without a service keyslot.
	svc2, err := auth.New(f.store, auth.WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	eng2 := New(f.store, svc2)
	defer eng2.Close()
	if svc2.Unlocked() {
		t.Fatal("the fresh Service is already unlocked; this cell cannot observe a locked store")
	}

	// THE ARRIVAL POINT, and it is NOT where ADR-0087 A1.3 said it was.
	//
	// A1.3 asserted the lock surfaces during the credential exchange, from a
	// source trace of openTarget. OpenWireSessionWith never opens a target, so
	// the session OPENS on a locked store and the FIRST STATEMENT is refused.
	// This cell pins both halves, because the front door's answer is built on
	// which of them is true.
	res2, oerr := eng2.OpenWireSessionWith(ctx, WireOpen{
		PAT: secret, StartupUser: "root", Database: dbName, IP: testIP,
	})
	if oerr != nil {
		t.Fatalf("opening a wire session on a LOCKED store failed with %v.\n\n"+
			"If that is a deliberate change — moving the target open into session "+
			"establishment — then the locked state has become PRE-AUTH and the front door's "+
			"57P03 must move with it: it is answered today from classifyGateError, which only "+
			"sees post-auth errors, so a locked store would silently go back to the uniform "+
			"28000 and developers would be told their credentials are wrong.", oerr)
	}

	// The first statement is where it is refused, and it must arrive UN-WRAPPED.
	_, qerr := eng2.WireQuery(ctx, res2.SessionID, res2.UserID, "SELECT 1", testIP,
		func(WireMessage) error { return nil })
	if !errors.Is(qerr, auth.ErrLocked) {
		t.Fatalf("the first query on a locked store returned %v, want ErrLocked — the front "+
			"door keys on it to answer 57P03", qerr)
	}
	// NO DENIAL REASON, which is the assertion that actually protects the
	// classification: frontdoor's post-auth path and its credential path both
	// branch on these, and an ErrLocked wrapped as a denial would be reported
	// as an authentication failure.
	if reason := DenialReason(qerr); reason != "" {
		t.Fatalf("the locked-store error carries denial reason %q, so the front door would "+
			"treat it as a credential refusal — a developer with a perfectly good token told "+
			"their credentials are wrong, which is what ADR-0087 §8 exists to prevent", reason)
	}
	eng2.CloseWireSession(ctx, res2.SessionID, res2.UserID, testIP, "cell done")

	// AND IT RECOVERS: a passphrase login opens the same path, so the refusal
	// is the lock rather than anything permanent about this engine.
	if _, _, err := svc2.Login(ctx, "root", "root-passphrase", testIP); err != nil {
		t.Fatal(err)
	}
	res3, err := eng2.OpenWireSessionWith(ctx, WireOpen{
		PAT: secret, StartupUser: "root", Database: dbName, IP: testIP,
	})
	if err != nil {
		t.Fatalf("after a passphrase login the wire session still would not open: %v", err)
	}
	if _, qerr := eng2.WireQuery(ctx, res3.SessionID, res3.UserID, "SELECT 1", testIP,
		func(WireMessage) error { return nil }); qerr != nil {
		t.Fatalf("after a passphrase login the first query still failed: %v", qerr)
	}
	eng2.CloseWireSession(ctx, res3.SessionID, res3.UserID, testIP, "cell done")
}
