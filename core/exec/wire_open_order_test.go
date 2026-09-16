package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE CREDENTIAL IS VERIFIED BEFORE THE BACKEND IS TOUCHED, AND A FAULT AT THE
// BACKEND IS NOT A CREDENTIAL FAILURE.
//
// WHY THIS CELL EXISTS IN THIS PACKAGE. The front door's version of it uses a
// fake authenticator, which records that OpenWireSession was CALLED and then
// returns its configured error. That proves invocation and non-charging; it
// cannot prove ORDER, because the fake never runs VerifyPAT at all. A front
// door cell therefore stays green if verification moves after the pin, or is
// skipped — which is precisely the arrangement that produced the incident.
//
// So this drives the real engine: a real store, a real PAT created through the
// real service, and the fault injected at the pin seam, which is downstream of
// VerifyPAT. The order is then observable rather than assumed.
func TestOpenWireSession_TheCredentialIsVerifiedBeforeTheBackendIsPinned(t *testing.T) {
	ctx := context.Background()
	f, _, secret, dbName := wireFixture(t)

	// The connection must speak the PostgreSQL wire, or no pin happens at
	// admission and the seam below is never reached — which is the whole
	// distinction this branch got wrong twice.
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnEngine, string(engine.Postgres)).Update(); err != nil {
		t.Fatalf("making the connection a postgres-wire one: %v", err)
	}

	var pinned int
	orig := pinSessionConn
	t.Cleanup(func() { pinSessionConn = orig })
	pinSessionConn = func(context.Context, dao.DataConn) (golibpg.PinnedConn, error) {
		pinned++
		return nil, errors.New("the pin failed after the credential was verified")
	}

	// THE CONTROL COMES FIRST. An INVALID credential must never reach the
	// seam: if it does, the fault is being decided before the credential is,
	// and a bad token would be answered with "this connection is unavailable"
	// — telling an attacker that their token got further than it did.
	_, berr := f.eng.OpenWireSessionWith(ctx, WireOpen{
		PAT: "adb_pat_bbbbbbbbbb.cccccccc", StartupUser: "root", Database: dbName, IP: testIP,
	})
	if berr == nil {
		t.Fatal("an invalid PAT opened a wire session")
	}
	if !errors.Is(berr, auth.ErrPATInvalid) && DenialReason(berr) == "" {
		t.Errorf("an invalid PAT returned %v, want a credential denial", berr)
	}
	if pinned != 0 {
		t.Fatalf("the pin seam was reached %d time(s) for an INVALID credential; "+
			"verification is not happening first, and a bad token is being answered "+
			"with a fact about the backend", pinned)
	}

	// NOW THE VALID CREDENTIAL. It must reach the seam — which is what proves
	// verification completed — and the resulting failure must not be a
	// credential denial.
	_, gerr := f.eng.OpenWireSessionWith(ctx, WireOpen{
		PAT: secret, StartupUser: "root", Database: dbName, IP: testIP,
	})
	if gerr == nil {
		t.Fatal("the injected pin fault did not fail the open")
	}
	if pinned != 1 {
		t.Fatalf("the pin seam was reached %d time(s) for a VALID credential, want 1: "+
			"either verification refused a good token, or the pin is not where this "+
			"cell believes it is", pinned)
	}
	if r := DenialReason(gerr); r != "" {
		t.Errorf("a backend fault after a VERIFIED credential was answered as the denial "+
			"%q; that is the lockout — a good token told it was bad", r)
	}
	if errors.Is(gerr, auth.ErrPATInvalid) {
		t.Error("a backend fault was reported as an invalid credential")
	}
	// And the error says nothing about the credential either.
	for _, tok := range []string{secret, "PAT", "token", "password"} {
		if tok != "" && strings.Contains(gerr.Error(), tok) {
			t.Errorf("the open error %q mentions %q; nothing about the credential is "+
				"wrong here", gerr.Error(), tok)
		}
	}
}
