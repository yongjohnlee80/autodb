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
	s := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}))
	return s, store, ck
}

func mustBootstrap(t *testing.T, s *Service) Identity {
	t.Helper()
	ident, err := s.Bootstrap(context.Background(), "root", rootPass, testIP)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return ident
}

func auditCount(t *testing.T, store *meta.Store, action string) uint64 {
	t.Helper()
	n, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func TestBootstrapAndLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)

	needs, err := s.NeedsBootstrap(ctx)
	if err != nil || !needs {
		t.Fatalf("NeedsBootstrap = %v, %v — want true", needs, err)
	}
	root := mustBootstrap(t, s)
	if root.Role != meta.RoleAdmin || !s.Unlocked() {
		t.Fatalf("bootstrap ident = %+v unlocked=%v", root, s.Unlocked())
	}
	if needs, _ := s.NeedsBootstrap(ctx); needs {
		t.Error("NeedsBootstrap still true after bootstrap")
	}
	if _, err := s.Bootstrap(ctx, "again", "whatever-pass", testIP); !errors.Is(err, ErrBootstrapDone) {
		t.Errorf("second Bootstrap err = %v, want ErrBootstrapDone", err)
	}

	token, ident, err := s.Login(ctx, "root", rootPass, testIP)
	if err != nil || ident.UserID != root.UserID {
		t.Fatalf("Login = %+v, %v", ident, err)
	}
	got, err := s.ValidateToken(ctx, token)
	if err != nil || got != ident {
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

func TestLockedUntilLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s1, store, ck := newSvc(t)
	root := mustBootstrap(t, s1)

	secret, err := s1.EncryptSecret([]byte("postgres://gold"))
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	// A fresh process over the same store starts locked.
	s2 := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if s2.Unlocked() {
		t.Fatal("fresh service is unlocked")
	}
	if _, err := s2.DecryptSecret(secret); !errors.Is(err, ErrLocked) {
		t.Errorf("DecryptSecret while locked err = %v, want ErrLocked", err)
	}
	if _, err := s2.CreateUser(ctx, root, "bob", "bob-pass-123", meta.RoleEditor, testIP); !errors.Is(err, ErrLocked) {
		t.Errorf("CreateUser while locked err = %v, want ErrLocked", err)
	}

	if _, _, err := s2.Login(ctx, "root", rootPass, testIP); err != nil {
		t.Fatalf("Login: %v", err)
	}
	got, err := s2.DecryptSecret(secret)
	if err != nil || !bytes.Equal(got, []byte("postgres://gold")) {
		t.Fatalf("post-login DecryptSecret = %q, %v", got, err)
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
	if err := s.RevokeUserSessions(ctx, ident, ident.UserID, testIP); err != nil {
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
	root := mustBootstrap(t, s)

	secret, err := s.EncryptSecret([]byte("dsn-before-rotations"))
	if err != nil {
		t.Fatal(err)
	}

	bobID, err := s.CreateUser(ctx, root, "bob", "bob-pass-old", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, bob, err := s.Login(ctx, "bob", "bob-pass-old", testIP)
	if err != nil {
		t.Fatalf("bob login: %v", err)
	}

	// Self-service rotation.
	if err := s.ChangePassphrase(ctx, bob, "bob-pass-old", "bob-pass-new", testIP); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-old", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old passphrase still works after change")
	}

	// The rewrapped keyslot still unwraps the SAME master key: a fresh
	// locked process unlocked by bob's NEW passphrase must decrypt secrets
	// sealed before the rotation.
	s2 := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}))
	if _, _, err := s2.Login(ctx, "bob", "bob-pass-new", testIP); err != nil {
		t.Fatalf("bob login after change on fresh service: %v", err)
	}
	if got, err := s2.DecryptSecret(secret); err != nil || !bytes.Equal(got, []byte("dsn-before-rotations")) {
		t.Fatalf("secret after rewrap = %q, %v", got, err)
	}

	// Admin recovery path.
	if err := s.ResetPassphrase(ctx, root, bobID, "bob-pass-reset", testIP); err != nil {
		t.Fatalf("ResetPassphrase: %v", err)
	}
	if _, _, err := s.Login(ctx, "bob", "bob-pass-reset", testIP); err != nil {
		t.Fatalf("bob login after admin reset: %v", err)
	}

	// Non-admin cannot reset others.
	if err := s.ResetPassphrase(ctx, bob, root.UserID, "hijack-pass", testIP); !errors.Is(err, ErrDenied) {
		t.Errorf("editor reset of root err = %v, want ErrDenied", err)
	}
}

func TestLastAdminGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	root := mustBootstrap(t, s)

	if err := s.SetUserRole(ctx, root, root.UserID, meta.RoleEditor, testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote sole admin err = %v, want ErrLastAdmin", err)
	}
	if err := s.SetUserDisabled(ctx, root, root.UserID, true, testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable sole admin err = %v, want ErrLastAdmin", err)
	}
	if err := s.RemoveUser(ctx, root, root.UserID, testIP); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("remove sole admin err = %v, want ErrLastAdmin", err)
	}

	if _, err := s.CreateUser(ctx, root, "second", "second-pass-1", meta.RoleAdmin, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserRole(ctx, root, root.UserID, meta.RoleEditor, testIP); err != nil {
		t.Errorf("demote with second admin present: %v", err)
	}
}

func TestIPAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	root := mustBootstrap(t, s)

	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("off-allowlist login err = %v, want ErrDenied", err)
	}
	if err := s.AddAllowedIP(ctx, root, "10.0.0.0/8", "vpn", testIP); err != nil {
		t.Fatalf("AddAllowedIP: %v", err)
	}
	if _, _, err := s.Login(ctx, "root", rootPass, "10.1.1.1"); err != nil {
		t.Fatalf("allowlisted login: %v", err)
	}
	if err := s.RemoveAllowedIP(ctx, root, "10.0.0.0/8", testIP); err != nil {
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
	root := mustBootstrap(t, s)

	connID, err := store.Connections.OnCtx(ctx).
		Set(meta.ConnName, "gold").Set(meta.ConnEngine, "postgres").
		Set(meta.ConnDSNEnc, []byte{1}).Set(meta.ConnCreatedBy, root.UserID).
		Set(meta.ConnCreatedAt, int64(1)).Set(meta.ConnUpdatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("connection: %v", err)
	}

	// One user per global role.
	ids := map[string]Identity{}
	for _, role := range []string{meta.RoleAdmin, meta.RoleEditor, meta.RoleReader} {
		uid, err := s.CreateUser(ctx, root, "u-"+role, "matrix-pass-1", role, testIP)
		if err != nil {
			t.Fatalf("CreateUser %s: %v", role, err)
		}
		ids[role] = Identity{UserID: uid, Name: "u-" + role, Role: role}
	}

	// No grant: every connection-scoped action denies, admins included
	// (Objective 13 — access comes only from grants).
	for role, ident := range ids {
		if err := s.Authorize(ctx, ident, connID, ActionRead); !errors.Is(err, ErrDenied) {
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
		ident := ids[tc.global]
		if err := s.AddGrant(ctx, root, ident.UserID, connID, tc.grant, testIP); err != nil {
			t.Fatalf("AddGrant: %v", err)
		}
		err := s.Authorize(ctx, ident, connID, tc.action)
		if tc.allow && err != nil {
			t.Errorf("global=%s grant=%s action=%s: err = %v, want allow", tc.global, tc.grant, tc.action, err)
		}
		if !tc.allow && !errors.Is(err, ErrDenied) {
			t.Errorf("global=%s grant=%s action=%s: err = %v, want ErrDenied", tc.global, tc.grant, tc.action, err)
		}
	}

	// Manage is global-role gated, connection-independent.
	if err := s.Authorize(ctx, ids[meta.RoleAdmin], 0, ActionManage); err != nil {
		t.Errorf("admin manage err = %v, want allow", err)
	}
	if err := s.Authorize(ctx, ids[meta.RoleEditor], 0, ActionManage); !errors.Is(err, ErrDenied) {
		t.Errorf("editor manage err = %v, want ErrDenied", err)
	}

	// RemoveGrant closes access.
	if err := s.RemoveGrant(ctx, root, ids[meta.RoleAdmin].UserID, connID, testIP); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	if err := s.Authorize(ctx, ids[meta.RoleAdmin], connID, ActionRead); !errors.Is(err, ErrDenied) {
		t.Errorf("post-revoke read err = %v, want ErrDenied", err)
	}
}
