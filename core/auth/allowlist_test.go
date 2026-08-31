package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// mkUser creates a user as root and logs them in, returning id + token.
func mkUser(t *testing.T, s *Service, rootTok, name, role string) (int64, string) {
	t.Helper()
	id, err := s.CreateUser(context.Background(), rootTok, name, name+"-passphrase", role, testIP)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", name, err)
	}
	tok, _, err := s.Login(context.Background(), name, name+"-passphrase", testIP)
	if err != nil {
		t.Fatalf("Login(%s): %v", name, err)
	}
	return id, tok
}

func TestListAllowedIPs_AdminSeesConfigAndStore_NonAdminDenied(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if err := s.AddAllowedIP(ctx, rootTok, "192.168.68.0/24", "lan", testIP); err != nil {
		t.Fatalf("AddAllowedIP: %v", err)
	}
	cfg, rows, err := s.ListAllowedIPs(ctx, rootTok)
	if err != nil {
		t.Fatalf("ListAllowedIPs: %v", err)
	}
	if len(cfg) != 1 || cfg[0] != "127.0.0.1/32" {
		t.Errorf("config CIDRs = %v, want the seeded loopback", cfg)
	}
	if len(rows) != 1 || rows[0].CIDR != "192.168.68.0/24" || rows[0].Note != "lan" {
		t.Errorf("store rows = %+v, want the added row", rows)
	}

	_, aliceTok := mkUser(t, s, rootTok, "alice", "editor")
	if _, _, err := s.ListAllowedIPs(ctx, aliceTok); !errors.Is(err, ErrDenied) {
		t.Errorf("non-admin ListAllowedIPs error = %v, want ErrDenied", err)
	}
}

func TestUserIP_AuthzMatrix(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	aliceID, aliceTok := mkUser(t, s, rootTok, "alice", "editor")
	bobID, _ := mkUser(t, s, rootTok, "bob", "reader")

	// Self-service: alice manages her own rows.
	if err := s.AddUserIP(ctx, aliceTok, aliceID, "10.1.2.3", "home", testIP); err != nil {
		t.Fatalf("alice AddUserIP(self): %v", err)
	}
	own, err := s.UserIPs(ctx, aliceTok, aliceID)
	if err != nil || len(own) != 1 {
		t.Fatalf("alice UserIPs(self) = %v rows, err %v; want 1", len(own), err)
	}

	// Cross-user, non-admin: denied on every verb — including LIST, so
	// probing another user's rows discloses nothing.
	if _, err := s.UserIPs(ctx, aliceTok, bobID); !errors.Is(err, ErrDenied) {
		t.Errorf("alice UserIPs(bob) error = %v, want ErrDenied", err)
	}
	if err := s.AddUserIP(ctx, aliceTok, bobID, "10.9.9.9", "x", testIP); !errors.Is(err, ErrDenied) {
		t.Errorf("alice AddUserIP(bob) error = %v, want ErrDenied", err)
	}
	if err := s.RemoveUserIP(ctx, aliceTok, bobID, 1, testIP); !errors.Is(err, ErrDenied) {
		t.Errorf("alice RemoveUserIP(bob) error = %v, want ErrDenied", err)
	}

	// Admin path: root manages anyone's rows.
	if err := s.AddUserIP(ctx, rootTok, bobID, "172.16.0.0/12", "vpn", testIP); err != nil {
		t.Fatalf("root AddUserIP(bob): %v", err)
	}
	bobRows, err := s.UserIPs(ctx, rootTok, bobID)
	if err != nil || len(bobRows) != 1 {
		t.Fatalf("root UserIPs(bob) = %v rows, err %v; want 1", len(bobRows), err)
	}
	if err := s.RemoveUserIP(ctx, rootTok, bobID, bobRows[0].ID, testIP); err != nil {
		t.Fatalf("root RemoveUserIP(bob): %v", err)
	}

	// Every mutation audited.
	if n := auditCount(t, store, "user_ip_added"); n != 2 {
		t.Errorf("user_ip_added audit rows = %d, want 2", n)
	}
	if n := auditCount(t, store, "user_ip_removed"); n != 1 {
		t.Errorf("user_ip_removed audit rows = %d, want 1", n)
	}
}

func TestAddUserIP_CanonicalizationAndValidation(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	// A bare address becomes its single-address prefix; an unmasked prefix
	// is masked to its network.
	if err := s.AddUserIP(ctx, rootTok, root.UserID(), "10.1.2.3", "", testIP); err != nil {
		t.Fatalf("bare address: %v", err)
	}
	if err := s.AddUserIP(ctx, rootTok, root.UserID(), "10.0.0.5/24", "", testIP); err != nil {
		t.Fatalf("unmasked prefix: %v", err)
	}
	rows, err := s.UserIPs(ctx, rootTok, root.UserID())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.CIDR] = true
	}
	if !got["10.1.2.3/32"] || !got["10.0.0.0/24"] {
		t.Errorf("canonical rows = %v, want 10.1.2.3/32 and 10.0.0.0/24", got)
	}

	if err := s.AddUserIP(ctx, rootTok, root.UserID(), "not-an-ip", "", testIP); err == nil {
		t.Error("invalid input accepted")
	}
	// Duplicate refused by UNIQUE(user_id, cidr).
	if err := s.AddUserIP(ctx, rootTok, root.UserID(), "10.1.2.3/32", "", testIP); err == nil {
		t.Error("duplicate CIDR accepted — UNIQUE(user_id, cidr) must refuse")
	}
}

func TestAddUserIP_CapRefusesLoudly(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()
	for i := 0; i < maxUserIPs; i++ {
		if err := s.AddUserIP(ctx, rootTok, root.UserID(),
			fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), "", testIP); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	err := s.AddUserIP(ctx, rootTok, root.UserID(), "192.0.2.0/24", "", testIP)
	if err == nil {
		t.Fatal("cap exceeded silently")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("cap refusal must name the cap; got %v", err)
	}
}

func TestRemoveUserIP_DeleteIsScopedToOwner(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	aliceID, _ := mkUser(t, s, rootTok, "alice", "editor")
	bobID, _ := mkUser(t, s, rootTok, "bob", "reader")

	if err := s.AddUserIP(ctx, rootTok, bobID, "10.7.7.7", "bobs", testIP); err != nil {
		t.Fatal(err)
	}
	bobRows, _ := s.UserIPs(ctx, rootTok, bobID)
	if len(bobRows) != 1 {
		t.Fatalf("bob rows = %d, want 1", len(bobRows))
	}

	// Even an ADMIN delete addressed to the wrong owner must not cross:
	// (id AND user_id) scoping makes the row id useless outside its user.
	if err := s.RemoveUserIP(ctx, rootTok, aliceID, bobRows[0].ID, testIP); err != nil {
		t.Fatalf("mismatched-owner delete errored (want silent no-op): %v", err)
	}
	after, _ := s.UserIPs(ctx, rootTok, bobID)
	if len(after) != 1 {
		t.Error("a delete scoped to alice removed bob's row — cross-user reach")
	}
}

func TestUserIPs_GoneWithTheUser(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	aliceID, _ := mkUser(t, s, rootTok, "alice", "editor")
	if err := s.AddUserIP(ctx, rootTok, aliceID, "10.5.5.5", "", testIP); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUser(ctx, rootTok, aliceID, testIP); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	orphans, err := store.UserIPs.OnCtx(ctx).With(meta.UIPUserID, aliceID).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("%d allowlist rows survived their user (ON DELETE CASCADE broken)", len(orphans))
	}
}
