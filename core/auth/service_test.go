package auth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

const (
	testIP   = "127.0.0.1"
	rootPass = "root-passphrase"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// newSvc opens an in-memory store and a Service with a controllable clock
// and the loopback allowlist.
func newSvc(t *testing.T) (*Service, *meta.Store, *clock) {
	t.Helper()
	store, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	s, err := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, store, ck
}

func svcOver(t *testing.T, store *meta.Store, ck *clock) *Service {
	t.Helper()
	s, err := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func mustBootstrap(t *testing.T, s *Service) (string, Identity) {
	t.Helper()
	token, ident, err := s.Bootstrap(context.Background(), "root", rootPass, testIP)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return token, ident
}

func auditCount(t *testing.T, store *meta.Store, action string) uint64 {
	t.Helper()
	n, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func TestNew_RejectsInvalidAllowlist(t *testing.T) {
	t.Parallel()
	store, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := New(store, WithConfigAllowlist([]string{"not-a-cidr"})); err == nil {
		t.Error("New accepted an invalid allowlist CIDR (silent narrowing)")
	}
}

func TestBootstrapAndLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)

	needs, err := s.NeedsBootstrap(ctx)
	if err != nil || !needs {
		t.Fatalf("NeedsBootstrap = %v, %v — want true", needs, err)
	}
	rootTok, root := mustBootstrap(t, s)
	if root.Role() != meta.RoleAdmin || !s.Unlocked() {
		t.Fatalf("bootstrap ident = %+v unlocked=%v", root, s.Unlocked())
	}
	// Bootstrap logs the root in: the returned token resolves.
	if got, err := s.ValidateToken(ctx, rootTok); err != nil || got.UserID() != root.UserID() {
		t.Fatalf("bootstrap token invalid: %+v, %v", got, err)
	}
	if _, _, err := s.Bootstrap(ctx, "again", "whatever-pass", testIP); !errors.Is(err, ErrBootstrapDone) {
		t.Errorf("second Bootstrap err = %v, want ErrBootstrapDone", err)
	}

	token, ident, err := s.Login(ctx, "root", rootPass, testIP)
	if err != nil || ident.UserID() != root.UserID() {
		t.Fatalf("Login = %+v, %v", ident, err)
	}
	got, err := s.ValidateToken(ctx, token)
	if err != nil || got.UserID() != ident.UserID() || got.Role() != meta.RoleAdmin {
		t.Fatalf("ValidateToken = %+v, %v", got, err)
	}

	if _, _, err := s.Login(ctx, "root", "wrong-passphrase", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong pass err = %v, want ErrBadCredentials", err)
	}
	if _, _, err := s.Login(ctx, "nobody", "whatever-pass", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("unknown user err = %v, want ErrBadCredentials", err)
	}
	if n := auditCount(t, store, "login_failed"); n != 2 {
		t.Errorf("login_failed audit rows = %d, want 2", n)
	}
	if n := auditCount(t, store, "bootstrap"); n != 1 {
		t.Errorf("bootstrap audit rows = %d, want 1", n)
	}
}

func TestAuthorityIsFreshPerCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	// A second admin so the demotion passes the last-admin guard.
	if _, err := s.CreateUser(ctx, rootTok, "second", "second-pass-1", meta.RoleAdmin, testIP); err != nil {
		t.Fatal(err)
	}
	secondTok, second, err := s.Login(ctx, "second", "second-pass-1", testIP)
	if err != nil {
		t.Fatal(err)
	}
	// second (admin) can create users right now.
	if _, err := s.CreateUser(ctx, secondTok, "temp1", "temp-pass-11", meta.RoleReader, testIP); err != nil {
		t.Fatalf("pre-demotion CreateUser: %v", err)
	}
	// Demote second; their EXISTING token must lose admin power immediately
	// (lector M3 must-fix #1 — no stale cached authority).
	if err := s.SetUserRole(ctx, rootTok, second.UserID(), meta.RoleReader, testIP); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	if _, err := s.CreateUser(ctx, secondTok, "temp2", "temp-pass-22", meta.RoleReader, testIP); !errors.Is(err, ErrDenied) {
		t.Errorf("post-demotion CreateUser err = %v, want ErrDenied", err)
	}
	if got, _ := s.ValidateToken(ctx, secondTok); got.Role() != meta.RoleReader {
		t.Errorf("resolved role = %s, want reader", got.Role())
	}
}

func TestLockedUntilLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s1, store, ck := newSvc(t)
	rootTok, _ := mustBootstrap(t, s1)

	secret, err := s1.EncryptSecret([]byte("postgres://gold"), 42)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	// A fresh process over the same store starts locked; sessions persist.
	s2 := svcOver(t, store, ck)
	if s2.Unlocked() {
		t.Fatal("fresh service is unlocked")
	}
	if _, err := s2.DecryptSecret(secret, 42); !errors.Is(err, ErrLocked) {
		t.Errorf("DecryptSecret while locked err = %v, want ErrLocked", err)
	}
	if _, err := s2.CreateUser(ctx, rootTok, "bob", "bob-pass-123", meta.RoleEditor, testIP); !errors.Is(err, ErrLocked) {
		t.Errorf("CreateUser while locked err = %v, want ErrLocked", err)
	}

	if _, _, err := s2.Login(ctx, "root", rootPass, testIP); err != nil {
		t.Fatalf("Login: %v", err)
	}
	got, err := s2.DecryptSecret(secret, 42)
	if err != nil || !bytes.Equal(got, []byte("postgres://gold")) {
		t.Fatalf("post-login DecryptSecret = %q, %v", got, err)
	}
}

func TestSecretAADBoundToConnection(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	mustBootstrap(t, s)

	blob, err := s.EncryptSecret([]byte("dsn-for-conn-1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	// The same ciphertext moved to another connection's row must not open
	// (lector M3 must-fix #5 — substitution).
	if _, err := s.DecryptSecret(blob, 2); err == nil {
		t.Error("ciphertext for conn 1 decrypted under conn 2 identity")
	}
	if got, err := s.DecryptSecret(blob, 1); err != nil || string(got) != "dsn-for-conn-1" {
		t.Errorf("legitimate decrypt = %q, %v", got, err)
	}
}

func TestTokenExpiryRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, ck := newSvc(t)
	mustBootstrap(t, s)

	token, _, err := s.Login(ctx, "root", rootPass, testIP)
	if err != nil {
		t.Fatal(err)
	}
	ck.t = ck.t.Add(DefaultSessionTTL + time.Minute)
	if _, err := s.ValidateToken(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expired token err = %v, want ErrTokenInvalid", err)
	}

	token2, ident, err := s.Login(ctx, "root", rootPass, testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(ctx, token2, testIP); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := s.ValidateToken(ctx, token2); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("revoked token err = %v, want ErrTokenInvalid", err)
	}

	token3, _, _ := s.Login(ctx, "root", rootPass, testIP)
	if err := s.RevokeUserSessions(ctx, token3, ident.UserID(), testIP); err != nil {
		t.Fatalf("RevokeUserSessions: %v", err)
	}
	if _, err := s.ValidateToken(ctx, token3); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("bulk-revoked token err = %v, want ErrTokenInvalid", err)
	}
}

func TestPassphraseChangeAndReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, ck := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	secret, err := s.EncryptSecret([]byte("dsn-before-rotations"), 7)
	if err != nil {
		t.Fatal(err)
	}

	bobID, err := s.CreateUser(ctx, rootTok, "bob", "bob-pass-old", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bobTok, _, err := s.Login(ctx, "bob", "bob-pass-old", testIP)
	if err != nil {
		t.Fatalf("bob login: %v", err)
	}
	bobTok2, _, err := s.Login(ctx, "bob", "bob-pass-old", testIP)
	if err != nil {
		t.Fatalf("bob second login: %v", err)
	}

	// Self-service rotation: keeps the calling session, revokes the others
	// (lector M3 should-fix).
	if err := s.ChangePassphrase(ctx, bobTok, "bob-pass-old", "bob-pass-new", testIP); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}
	if _, err := s.ValidateToken(ctx, bobTok); err != nil {
		t.Errorf("calling session revoked by own passphrase change: %v", err)
	}
	if _, err := s.ValidateToken(ctx, bobTok2); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("other session survived passphrase change: %v", err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-old", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old passphrase still works after change")
	}

	// The rewrapped keyslot still unwraps the SAME master key: a fresh
	// locked process unlocked by bob's NEW passphrase must decrypt secrets
	// sealed before the rotation.
	s2 := svcOver(t, store, ck)
	if _, _, err := s2.Login(ctx, "bob", "bob-pass-new", testIP); err != nil {
		t.Fatalf("bob login after change on fresh service: %v", err)
	}
	if got, err := s2.DecryptSecret(secret, 7); err != nil || !bytes.Equal(got, []byte("dsn-before-rotations")) {
		t.Fatalf("secret after rewrap = %q, %v", got, err)
	}

	// Admin recovery path.
	if err := s.ResetPassphrase(ctx, rootTok, bobID, "bob-pass-reset", testIP); err != nil {
		t.Fatalf("ResetPassphrase: %v", err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-reset", testIP); err != nil {
		t.Fatalf("bob login after admin reset: %v", err)
	}

	// Non-admin cannot reset others.
	bobTok3, _, _ := s.Login(ctx, "bob", "bob-pass-reset", testIP)
	rootID := int64(1)
	if err := s.ResetPassphrase(ctx, bobTok3, rootID, "hijack-pass", testIP); !errors.Is(err, ErrDenied) {
		t.Errorf("editor reset of root err = %v, want ErrDenied", err)
	}
}

func TestNoKeyslotFailsExplicitly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	id, err := s.CreateUser(ctx, rootTok, "legacy", "legacy-pass-1", meta.RoleReader, testIP)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-v2 row whose keyslot was never cut.
	if err := store.Users.OnCtx(ctx).With(meta.UserID, id).
		Set(meta.UserMKWrapped, []byte{}).Update(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "legacy", "legacy-pass-1", testIP); !errors.Is(err, ErrNoKeyslot) {
		t.Errorf("empty keyslot login err = %v, want ErrNoKeyslot", err)
	}
	// Admin reset cuts a fresh keyslot; login then works.
	if err := s.ResetPassphrase(ctx, rootTok, id, "legacy-pass-2", testIP); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "legacy", "legacy-pass-2", testIP); err != nil {
		t.Errorf("login after keyslot adoption: %v", err)
	}
}

func TestLastAdminGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)

	if err := s.SetUserRole(ctx, rootTok, root.UserID(), meta.RoleEditor, testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote sole admin err = %v, want ErrLastAdmin", err)
	}
	if err := s.SetUserDisabled(ctx, rootTok, root.UserID(), true, testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable sole admin err = %v, want ErrLastAdmin", err)
	}
	if err := s.RemoveUser(ctx, rootTok, root.UserID(), testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("remove sole admin err = %v, want ErrLastAdmin", err)
	}

	if _, err := s.CreateUser(ctx, rootTok, "second", "second-pass-1", meta.RoleAdmin, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserRole(ctx, rootTok, root.UserID(), meta.RoleEditor, testIP); err != nil {
		t.Errorf("demote with second admin present: %v", err)
	}
}

func TestIPAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("off-allowlist login err = %v, want ErrDenied", err)
	}
	if err := s.AddAllowedIP(ctx, rootTok, "10.0.0.0/8", "vpn", testIP); err != nil {
		t.Fatalf("AddAllowedIP: %v", err)
	}
	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); err != nil {
		t.Fatalf("allowlisted login: %v", err)
	}
	if err := s.RemoveAllowedIP(ctx, rootTok, "10.0.0.0/8", testIP); err != nil {
		t.Fatalf("RemoveAllowedIP: %v", err)
	}
	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); !errors.Is(err, ErrDenied) {
		t.Errorf("post-removal login err = %v, want ErrDenied", err)
	}
}

func TestAuthorizeMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)

	connID, err := store.Connections.OnCtx(ctx).
		Set(meta.ConnName, "gold").Set(meta.ConnEngine, "postgres").
		Set(meta.ConnDSNEnc, []byte{1}).Set(meta.ConnCreatedBy, root.UserID()).
		Set(meta.ConnCreatedAt, int64(1)).Set(meta.ConnUpdatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("connection: %v", err)
	}

	// One user (and live token) per global role.
	tokens := map[string]string{}
	userIDs := map[string]int64{}
	for _, role := range []string{meta.RoleAdmin, meta.RoleEditor, meta.RoleReader} {
		uid, err := s.CreateUser(ctx, rootTok, "u-"+role, "matrix-pass-1", role, testIP)
		if err != nil {
			t.Fatalf("CreateUser %s: %v", role, err)
		}
		tok, _, err := s.Login(ctx, "u-"+role, "matrix-pass-1", testIP)
		if err != nil {
			t.Fatalf("login %s: %v", role, err)
		}
		tokens[role], userIDs[role] = tok, uid
	}

	// No grant: every connection-scoped action denies, admins included
	// (Objective 13 — access comes only from grants).
	for role, tok := range tokens {
		if _, err := s.Authorize(ctx, tok, connID, ActionRead); !errors.Is(err, ErrDenied) {
			t.Errorf("no-grant %s read err = %v, want ErrDenied", role, err)
		}
	}

	// effective = min(global, grant); read needs reader+, write/ddl editor+.
	cases := []struct {
		global, grant string
		action        Action
		allow         bool
	}{
		{meta.RoleAdmin, meta.RoleAdmin, ActionRead, true},
		{meta.RoleAdmin, meta.RoleAdmin, ActionDDL, true},
		{meta.RoleAdmin, meta.RoleReader, ActionRead, true},
		{meta.RoleAdmin, meta.RoleReader, ActionWrite, false},
		{meta.RoleEditor, meta.RoleEditor, ActionWrite, true},
		{meta.RoleEditor, meta.RoleEditor, ActionDDL, true},
		{meta.RoleEditor, meta.RoleAdmin, ActionDDL, true}, // eff = editor
		{meta.RoleEditor, meta.RoleReader, ActionWrite, false},
		{meta.RoleReader, meta.RoleEditor, ActionRead, true},
		{meta.RoleReader, meta.RoleEditor, ActionWrite, false}, // Objective 15: reader caps at read
		{meta.RoleReader, meta.RoleAdmin, ActionDDL, false},
		{meta.RoleReader, meta.RoleReader, ActionRead, true},
	}
	for _, tc := range cases {
		if err := s.AddGrant(ctx, rootTok, userIDs[tc.global], connID, tc.grant, testIP); err != nil {
			t.Fatalf("AddGrant: %v", err)
		}
		_, err := s.Authorize(ctx, tokens[tc.global], connID, tc.action)
		if tc.allow && err != nil {
			t.Errorf("global=%s grant=%s action=%s: err = %v, want allow", tc.global, tc.grant, tc.action, err)
		}
		if !tc.allow && !errors.Is(err, ErrDenied) {
			t.Errorf("global=%s grant=%s action=%s: err = %v, want ErrDenied", tc.global, tc.grant, tc.action, err)
		}
	}

	// Manage is global-role gated, connection-independent.
	if _, err := s.Authorize(ctx, tokens[meta.RoleAdmin], 0, ActionManage); err != nil {
		t.Errorf("admin manage err = %v, want allow", err)
	}
	if _, err := s.Authorize(ctx, tokens[meta.RoleEditor], 0, ActionManage); !errors.Is(err, ErrDenied) {
		t.Errorf("editor manage err = %v, want ErrDenied", err)
	}

	// RemoveGrant closes access.
	if err := s.RemoveGrant(ctx, rootTok, userIDs[meta.RoleAdmin], connID, testIP); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	if _, err := s.Authorize(ctx, tokens[meta.RoleAdmin], connID, ActionRead); !errors.Is(err, ErrDenied) {
		t.Errorf("post-revoke read err = %v, want ErrDenied", err)
	}
}

// A login whose credentials are reset before its session commits must fail:
// the credential is re-verified inside the committing tx (lector M3 r3
// must-fix). Non-concurrent proxy — an admin reset then an old-passphrase
// login — exercises the same recheck path.
func TestLogin_OldCredentialsAfterResetRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	uid, err := s.CreateUser(ctx, rootTok, "bob", "bob-pass-old", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassphrase(ctx, rootTok, uid, "bob-pass-new", testIP); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-old", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old-passphrase login after reset err = %v, want ErrBadCredentials", err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-new", testIP); err != nil {
		t.Errorf("new-passphrase login after reset: %v", err)
	}
}

// TestLoginLocalSocketBypassesAllowlist reproduces the field bug where a
// login over the default unix socket was refused "ip not allowed": a
// socket peer has no IP, so it can satisfy no IP allowlist. The socket's
// 0600 perms are the boundary (ADR-0058), so a local connection must not
// be gated on the allowlist — while TCP still is.
func TestLoginLocalSocketBypassesAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t) // config allowlist is 127.0.0.1/32 only
	mustBootstrap(t, s)

	if _, _, err := s.Login(ctx, "root", rootPass, LocalPeer); err != nil {
		t.Fatalf("local-socket login = %v, want success", err)
	}

	// The allowlist still governs TCP: a real, off-list IP is denied.
	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("off-allowlist TCP login = %v, want ErrDenied", err)
	}

	// Only the exact sentinel bypasses. A stray unparseable string — e.g.
	// the raw "@" an unnamed socket used to leak through peerIP — is NOT
	// silently treated as local.
	if _, _, err := s.Login(ctx, "root", rootPass, "@"); !errors.Is(err, ErrDenied) {
		t.Fatalf("bogus peer %q = %v, want ErrDenied", "@", err)
	}
}
