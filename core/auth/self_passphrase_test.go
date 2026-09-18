package auth

// CHANGING YOUR OWN PASSPHRASE IS NOT AN ADMINISTRATIVE ACT.
//
// ChangePassphrase resolves the CALLER's token and acts on that actor's own
// row. It has no role check at all — deliberately, because an account's
// passphrase belongs to whoever holds the account, and a deployment where a
// reader must find an administrator to rotate their own credential is a
// deployment where credentials do not get rotated.
//
// THE LOWEST ROLE IS THE CELL. An editor already changes its own passphrase in
// TestPassphraseChangeAndReset, but incidentally: that test is about the change
// and reset verbs, not about who may call them, and a role gate added later
// would still let an editor through if it admitted anything above reader. The
// reader is the strongest statement of "non-admin".
//
// Its sibling is the contrast that makes the claim mean something: ResetPassphrase
// — pointing at somebody ELSE's account — IS admin-only, and a reader is refused.

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

func TestSelfPassphrase_AReaderMayChangeItsOwnWithoutAnAdministrator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	if _, err := s.CreateUser(ctx, rootTok, "rhea", "rhea-pass-old", meta.RoleReader, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	readerTok, _, err := s.Login(ctx, "rhea", "rhea-pass-old", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := s.ChangePassphrase(ctx, readerTok, "rhea-pass-old", "rhea-pass-new", testIP); err != nil {
		t.Fatalf("a reader could not change its OWN passphrase: %v\n\n"+
			"This is self-service by design. If a role gate has been added here, a reader now "+
			"needs an administrator to rotate their own credential, which is how credentials "+
			"stop being rotated at all", err)
	}

	// IT REALLY CHANGED, both directions. A no-op that returns nil would pass
	// the assertion above.
	if _, _, err := s.Login(ctx, "rhea", "rhea-pass-new", testIP); err != nil {
		t.Errorf("the new passphrase does not authenticate: %v", err)
	}
	if _, _, err := s.Login(ctx, "rhea", "rhea-pass-old", testIP); err == nil {
		t.Error("the OLD passphrase still authenticates; the change did not replace it")
	}
}

// AND THE CONTRAST: POINTING AT SOMEBODY ELSE IS ADMIN-ONLY.
//
// Without this the cell above would be consistent with a service that let any
// caller rewrite any account, which is the opposite of the property claimed.
func TestSelfPassphrase_AReaderMayNotResetAnotherAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	victimID, err := s.CreateUser(ctx, rootTok, "victim", "victim-pass-old", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatalf("CreateUser(victim): %v", err)
	}
	if _, err := s.CreateUser(ctx, rootTok, "rhea", "rhea-pass-old", meta.RoleReader, testIP); err != nil {
		t.Fatalf("CreateUser(rhea): %v", err)
	}
	readerTok, _, err := s.Login(ctx, "rhea", "rhea-pass-old", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	err = s.ResetPassphrase(ctx, readerTok, victimID, "attacker-chosen", testIP)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a reader reset ANOTHER account's passphrase (err = %v, want ErrDenied); "+
			"self-service must not have widened into administering other people's "+
			"credentials", err)
	}
	// The victim's own passphrase is untouched.
	if _, _, lerr := s.Login(ctx, "victim", "victim-pass-old", testIP); lerr != nil {
		t.Errorf("the refused reset still changed the victim's passphrase: %v", lerr)
	}
}
