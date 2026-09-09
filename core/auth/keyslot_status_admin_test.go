package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE KEYSLOT STATUS IS ADMIN-ONLY, AND THE SERVER IS WHERE THAT IS DECIDED.
//
// The RPC verb was gated on ValidateToken alone, so any authenticated editor
// could read ServiceKeyslotState.Reason -- an err.Error() from the boot unlock
// attempt that names the configured keyfile PATH and distinguishes absent from
// wrong-mode from corrupt. That is detail about how this install protects its
// master key: deny before you disclose. A reason this specific is admissible
// precisely BECAUSE only an administrator can read it.
//
// Hiding the menu entry is not this fix; this is.
func TestServiceKeyslotStatusFor_AdminOnly(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	// A POSITIVE CONTROL FIRST. Without it a method that denied everyone
	// would satisfy every assertion below.
	if _, err := s.ServiceKeyslotStatusFor(ctx, rootTok); err != nil {
		t.Fatalf("an admin could not read the keyslot status: %v", err)
	}

	if _, err := s.CreateUser(ctx, rootTok, "eve", "eve-passphrase-long", meta.RoleEditor, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	eveTok, _, err := s.Login(ctx, "eve", "eve-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServiceKeyslotStatusFor(ctx, eveTok); !errors.Is(err, ErrDenied) {
		t.Errorf("an EDITOR read the keyslot status: %v", err)
	}

	// Unauthenticated: a garbage token must not reach the state either, and
	// must not be told it exists.
	if _, err := s.ServiceKeyslotStatusFor(ctx, "adb_pat_not-a-real-token"); err == nil {
		t.Error("an unauthenticated caller read the keyslot status")
	}
}

// A DEMOTED ADMIN LOSES IT IMMEDIATELY, even holding a token minted while they
// were still an admin.
//
// This is the cell for the client's cached role: the TUI knows a role from
// login and keeps it, so hiding a menu entry can never be the boundary. The
// check has to resolve the role from the store on every call, and this proves
// it does.
func TestServiceKeyslotStatusFor_DemotionTakesEffectOnTheNextCall(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newKeyslotFixture(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	mallory, err := s.CreateUser(ctx, rootTok, "mallory", "mallory-passphrase-long", meta.RoleAdmin, testIP)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, _, lerr := s.Login(ctx, "mallory", "mallory-passphrase-long", testIP)
	if lerr != nil {
		t.Fatal(lerr)
	}
	// While still an admin the same token WORKS — so the denial below is
	// caused by the demotion and nothing else.
	if _, err := s.ServiceKeyslotStatusFor(ctx, tok); err != nil {
		t.Fatalf("an admin token was refused before any demotion: %v", err)
	}

	if err := s.SetUserRole(ctx, rootTok, mallory, meta.RoleEditor, testIP); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}

	if _, err := s.ServiceKeyslotStatusFor(ctx, tok); !errors.Is(err, ErrDenied) {
		t.Errorf("a demoted admin still read the keyslot status with the old token: %v", err)
	}
}

// And the reason really is the disclosure this is about — if Reason stopped
// carrying the keyfile path, the gate would still be right but this cell's
// premise would be stale, and a reader deserves to know which.
func TestServiceKeyslotState_ReasonCarriesOperationalDetail(t *testing.T) {
	t.Parallel()
	s, _, _, keyfile := newKeyslotFixture(t)
	ctx := context.Background()

	// No slot was ever cut, so the boot probe fails and records why.
	if err := s.UnlockWithServiceKeyslot(ctx); err == nil {
		t.Fatal("unlocking from an unenrolled keyslot succeeded")
	}
	st := s.ServiceKeyslotStatus()
	if !strings.Contains(st.Reason, keyfile) {
		t.Skipf("Reason no longer names the keyfile path (%q); the admin gate is still "+
			"correct, but this cell's premise about WHAT is disclosed is stale", st.Reason)
	}
}
