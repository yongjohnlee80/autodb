package auth

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

func seedUserIP(t *testing.T, s *Service, userID int64, cidr string) {
	t.Helper()
	if _, err := s.store.UserIPs.OnCtx(context.Background()).
		Set(meta.UIPUserID, userID).Set(meta.UIPCIDR, cidr).
		Set(meta.UIPLabel, "seed").Set(meta.UIPCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("seeding %s: %v", cidr, err)
	}
}

// Amendment 1: admission is (global OR the user's own rows). Under the AND
// this replaced, a colleague at an already-listed office still needed a
// personal row, and a home address had to be listed GLOBALLY to be usable —
// which bloats the perimeter for everyone and makes the per-user layer a
// second registration rather than a narrowing.
func TestIPAllowedForUser_EitherLayerAdmits(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t) // config allowlist is 127.0.0.1/32
	_, ident := mustBootstrap(t, s)
	ctx := context.Background()
	uid := ident.UserID()

	// The GLOBAL layer alone admits, with no user row at all. This is the
	// case AND got wrong: the shared office address works for everyone
	// without each person registering it.
	src, err := s.IPAllowedForUser(ctx, nil, uid, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if src != AdmittedByGlobal {
		t.Errorf("a globally-listed address gave %q, want %q — a colleague at a listed office "+
			"must not need a personal row", src, AdmittedByGlobal)
	}

	// An address in NEITHER layer is refused. Without this the test above
	// would pass against a predicate that admits everything.
	if src, _ := s.IPAllowedForUser(ctx, nil, uid, "203.0.113.9"); src != NotAdmitted {
		t.Fatalf("an unlisted address gave %q, want %q — this test cannot observe an admission "+
			"decision at all otherwise", src, NotAdmitted)
	}

	// The USER layer alone admits, with the address absent from the global
	// list. This is the case that lets a home address stay personal.
	seedUserIP(t, s, uid, "203.0.113.0/24")
	src, err = s.IPAllowedForUser(ctx, nil, uid, "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if src != AdmittedByUserRow {
		t.Errorf("an address in the user's own rows gave %q, want %q", src, AdmittedByUserRow)
	}
}

// A row belonging to one user does not admit another. The whole point of the
// per-user layer is that a dev's home address admits THAT dev.
func TestIPAllowedForUser_RowsAreNotShared(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, rootTok, "dave", "dave-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	dave, err := store.Users.OnCtx(ctx).With(meta.UserName, "dave").Get()
	if err != nil {
		t.Fatal(err)
	}
	seedUserIP(t, s, root.UserID(), "198.51.100.0/24")

	if src, _ := s.IPAllowedForUser(ctx, nil, root.UserID(), "198.51.100.5"); src != AdmittedByUserRow {
		t.Fatalf("the owner was not admitted by their own row (%q)", src)
	}
	if src, _ := s.IPAllowedForUser(ctx, nil, dave.ID, "198.51.100.5"); src != NotAdmitted {
		t.Errorf("another user was admitted by someone else's row (%q); a dev's home address "+
			"must admit THAT dev and no one else", src)
	}
}

// A token's allowed_ips NARROWS the admission set. Empty inherits — an empty
// list that denied everything would make the ordinary token useless.
func TestPATAllowsIP_NarrowsButEmptyInherits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		allowed string
		ip      string
		want    bool
	}{
		{"empty inherits", "", "203.0.113.9", true},
		{"whitespace is empty", "   ", "203.0.113.9", true},
		{"inside the token's prefix", "203.0.113.0/24", "203.0.113.9", true},
		{"outside it", "203.0.113.0/24", "198.51.100.5", false},
		{"one of several", "10.0.0.0/8,203.0.113.0/24", "203.0.113.9", true},
		{"in none of several", "10.0.0.0/8,203.0.113.0/24", "192.0.2.1", false},
		{"a malformed entry does not admit", "not-a-cidr", "203.0.113.9", false},
		{"an unparseable address is refused", "203.0.113.0/24", "not-an-ip", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PATAllowsIP(tc.allowed, tc.ip); got != tc.want {
				t.Errorf("PATAllowsIP(%q, %q) = %v, want %v", tc.allowed, tc.ip, got, tc.want)
			}
		})
	}
}
