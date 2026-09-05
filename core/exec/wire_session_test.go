package exec

import (
	"context"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// wireFixture builds an engine with a front-door-enabled connection, a user
// with a grant, an allowlisted address and a PAT.
func wireFixture(t *testing.T) (*fixture, *meta.PAT, string, string) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnProfile, string(ProfileSession)).Update(); err != nil {
		t.Fatalf("enabling the session profile: %v", err)
	}
	row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
	if err != nil {
		t.Fatal(err)
	}
	newPAT, err := f.svc.CreatePAT(ctx, f.rootTok, "wire", f.connID, 0, nil)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	sel, _, _ := splitPATForTest(newPAT.Secret)
	pat, err := f.store.PATs.OnCtx(ctx).With(meta.PATSelector, sel).Get()
	if err != nil {
		t.Fatal(err)
	}
	return f, pat, newPAT.Secret, row.Name
}

func splitPATForTest(token string) (string, string, bool) {
	rest, ok := strings.CutPrefix(token, auth.PATPrefix)
	if !ok {
		return "", "", false
	}
	sel, sec, ok := strings.Cut(rest, ".")
	return sel, sec, ok
}

// Matrix row 2.7's chain, end to end. The happy path FIRST: without it every
// refusal below could be a function that refuses everything.
func TestOpenWireSession_AuthenticatesAndReserves(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()

	got, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("a valid front-door connection was refused: %v (%s)", err, DenialReason(err))
	}
	if got.SessionID == "" || got.ConnID != f.connID {
		t.Fatalf("result = %+v", got)
	}
	if got.PATName != "wire" {
		t.Errorf("PATName = %q — the audit must record which credential authenticated", got.PATName)
	}
	if got.AdmissionSource != auth.AdmittedByGlobal {
		t.Errorf("AdmissionSource = %q; the audit must say WHICH layer admitted the address",
			got.AdmissionSource)
	}
	// The reservation was taken: a lease is held on the target.
	if n := f.eng.sessions.leaseCount(f.connID); n != 1 {
		t.Errorf("leases on the target = %d, want 1 — the session was admitted without one", n)
	}
	if f.eng.sessions.residentHeld() != WireSessionOverhead {
		t.Errorf("resident = %d, want the session's fixed overhead %d",
			f.eng.sessions.residentHeld(), WireSessionOverhead)
	}

	// And closing gives it all back.
	f.eng.CloseWireSession(ctx, got.SessionID, got.UserID, testIP, "test")
	if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
		t.Errorf("leases after close = %d, want 0", n)
	}
	if f.eng.sessions.residentHeld() != 0 {
		t.Errorf("resident after close = %d, want 0", f.eng.sessions.residentHeld())
	}
}

// Every refusal is a DISTINCT internal reason and the SAME denial to the
// caller. A client that could tell "no such database" from "no grant" from
// "wrong token" could map the install without ever holding a credential.
func TestOpenWireSession_EveryRefusalIsAuditedDistinctlyAndDeniedUniformly(t *testing.T) {
	t.Parallel()
	f, pat, secret, dbName := wireFixture(t)
	ctx := context.Background()

	// A second connection with no grant and no front-door profile, for the
	// target-side refusals.
	otherID, err := f.eng.CreateConnection(ctx, f.rootTok, "no-frontdoor", "sqlite",
		"file:wirenofd?mode=memory&cache=shared", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	otherRow, _ := f.store.Connections.OnCtx(ctx).With(meta.ConnID, otherID).Get()

	for _, tc := range []struct {
		name   string
		run    func() error
		reason string
	}{
		{"a credential that does not verify", func() error {
			_, e := f.eng.OpenWireSession(ctx, auth.PATPrefix+"aaaa.bbbb", "root", dbName, testIP)
			return e
		}, DenyBadCredential},
		// Matrix row 3.1:user#owner-cross-check: the startup user must match
		// the PAT owner; mismatch is refused without disclosing the cause.
		{"the startup user is not the token's owner", func() error {
			_, e := f.eng.OpenWireSession(ctx, secret, "someone-else", dbName, testIP)
			return e
		}, DenyUserMismatch},
		{"an address in neither allowlist layer", func() error {
			_, e := f.eng.OpenWireSession(ctx, secret, "root", dbName, "203.0.113.7")
			return e
		}, DenyIPNotAdmitted},
		{"a database that does not exist", func() error {
			_, e := f.eng.OpenWireSession(ctx, secret, "root", "no-such-db", testIP)
			return e
		}, DenyNoSuchDatabase},
		{"a connection whose profile refuses the front door", func() error {
			_, e := f.eng.OpenWireSession(ctx, secret, "root", otherRow.Name, testIP)
			return e
		}, DenyProfileRefuses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s was ACCEPTED", tc.name)
			}
			if got := DenialReason(err); got != tc.reason {
				t.Errorf("audited reason = %q, want %q — the trail must distinguish what the "+
					"wire deliberately cannot", got, tc.reason)
			}
			// Nothing was reserved by a refusal.
			if n := f.eng.sessions.leaseCount(f.connID); n != 0 {
				t.Errorf("a refused connection left %d lease(s) behind", n)
			}
		})
	}
	_ = pat
}

// A token whose own allowed_ips excludes the address is refused even though
// the admission set would have allowed it — that narrowing is the mitigation
// for Amendment 1's accepted cost.
func TestOpenWireSession_PATNarrowingIsEnforced(t *testing.T) {
	t.Parallel()
	f, _, _, dbName := wireFixture(t)
	ctx := context.Background()

	// A row so the subset check has something to be a subset OF, then a
	// token narrowed to a range the test address is not in.
	if _, err := f.store.UserIPs.OnCtx(ctx).
		Set(meta.UIPUserID, userIDOf(t, f)).Set(meta.UIPCIDR, "10.0.0.0/8").
		Set(meta.UIPLabel, "vpn").Set(meta.UIPCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatal(err)
	}
	narrowed, err := f.svc.CreatePAT(ctx, f.rootTok, "narrowed", f.connID, 0, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}

	// testIP is admitted GLOBALLY, so admission passes — and the token's own
	// narrowing must still refuse it.
	_, err = f.eng.OpenWireSession(ctx, narrowed.Secret, "root", dbName, testIP)
	if err == nil {
		t.Fatal("a token narrowed to 10.1.0.0/16 authenticated from a loopback address; the " +
			"per-token narrowing is the mitigation for a stolen PAT working from any " +
			"globally-listed address, and it did not apply")
	}
	if got := DenialReason(err); got != DenyPATIPNarrowed {
		t.Errorf("reason = %q, want %q", got, DenyPATIPNarrowed)
	}
}

// The lease cap refuses with its OWN audit identity while the wire sees the
// same denial as everything else (matrix row 2.7, ruling 4).
func TestOpenWireSession_LeaseCapHasItsOwnAuditIdentity(t *testing.T) {
	t.Parallel()
	f, _, secret, dbName := wireFixture(t)
	ctx := context.Background()
	f.eng.sessions.leaseCap = 1

	first, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err = f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err == nil {
		t.Fatal("a second wire session was admitted past a lease cap of 1")
	}
	if got := DenialReason(err); got != DenyLeaseCap {
		t.Errorf("reason = %q, want %q — the operator's remedy for a full target pool differs "+
			"from a full session cap, so the trail must say which it was", got, DenyLeaseCap)
	}
	// Closing the first frees the lease for the next caller.
	f.eng.CloseWireSession(ctx, first.SessionID, first.UserID, testIP, "test")
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Errorf("closing a wire session did not free its lease: %v", err)
	}
}

// The three refusals the first version of this file declared and never
// exercised. My own unused-reason check found them, which is the point of
// running it before asking for review rather than after: a named reason with
// no cell is a branch nobody has ever seen execute.
func TestOpenWireSession_TheRemainingRefusals(t *testing.T) {
	t.Parallel()

	// Matrix row 3.1:database#grant-on-target: authentication does not grant
	// access to a presented target unless the user has an explicit grant.
	t.Run("a user with no grant on the target", func(t *testing.T) {
		t.Parallel()
		f, _, _, dbName := wireFixture(t)
		ctx := context.Background()

		// A second user who HOLDS a grant, mints a token, and then LOSES the
		// grant. Root is an admin with grants everywhere, which is why the
		// first version of this file could not reach this branch at all.
		//
		// The grant-then-revoke shape is required as of ADR-0086: a PAT is
		// bound to a connection at mint, and minting is refused for a
		// connection the caller has no grant on. So "a token whose owner has
		// no grant" can no longer be built by minting without one — which is
		// the point of the gate, and makes this the faithful scenario rather
		// than a workaround: a grant is revoked while a credential for it is
		// still in someone's DSN.
		if _, err := f.svc.CreateUser(ctx, f.rootTok, "erin", "erin-passphrase-long", "editor", testIP); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		erinTok, erinIdent, err := f.svc.Login(ctx, "erin", "erin-passphrase-long", testIP)
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		if gerr := f.svc.AddGrant(ctx, f.rootTok, erinIdent.UserID(), f.connID, "reader", testIP); gerr != nil {
			t.Fatalf("AddGrant: %v", gerr)
		}
		erinPAT, err := f.svc.CreatePAT(ctx, erinTok, "erins", f.connID, 0, nil)
		if err != nil {
			t.Fatalf("CreatePAT: %v", err)
		}
		// Revoked AFTER the mint: the session-open path must re-resolve
		// authority rather than trust the binding recorded on the token.
		if rerr := f.svc.RemoveGrant(ctx, f.rootTok, erinIdent.UserID(), f.connID, testIP); rerr != nil {
			t.Fatalf("RemoveGrant: %v", rerr)
		}

		_, err = f.eng.OpenWireSession(ctx, erinPAT.Secret, "erin", dbName, testIP)
		if err == nil {
			t.Fatal("a user with no grant on the target authenticated to it")
		}
		if got := DenialReason(err); got != DenyNoGrant {
			t.Errorf("reason = %q, want %q", got, DenyNoGrant)
		}
	})

	t.Run("the per-user session cap", func(t *testing.T) {
		t.Parallel()
		f, _, secret, dbName := wireFixture(t)
		ctx := context.Background()
		f.eng.sessions.perUserCap = 1

		if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
			t.Fatalf("first: %v", err)
		}
		_, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
		if err == nil {
			t.Fatal("a second session was admitted past a per-user cap of 1")
		}
		if got := DenialReason(err); got != DenySessionCap {
			t.Errorf("reason = %q, want %q", got, DenySessionCap)
		}
	})

	t.Run("the global resident-memory budget", func(t *testing.T) {
		t.Parallel()
		f, _, secret, dbName := wireFixture(t)
		ctx := context.Background()
		// Room for one session's overhead and no more.
		f.eng.sessions.residentCap = WireSessionOverhead

		if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
			t.Fatalf("first: %v", err)
		}
		_, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
		if err == nil {
			t.Fatal("a second session was admitted past the resident budget")
		}
		if got := DenialReason(err); got != DenyResidentBudget {
			t.Errorf("reason = %q, want %q — the memory budget is the fourth reservation "+
				"member and must refuse on its own", got, DenyResidentBudget)
		}
	})
}
