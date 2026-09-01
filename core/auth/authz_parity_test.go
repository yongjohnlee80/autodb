package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// One authorization rule, reached from both entry points (lector PR #33 r0
// must-fix 1).
//
// Authorize starts from a session token; AuthorizeUser starts from a user id
// the front door resolved through a PAT. What legitimately differs is the
// RESOLUTION — a session has a row with an expiry, a PAT does not. What must
// never differ is the decision that follows.
//
// The first version wrote that decision twice, under a comment saying there
// was one place. The comment was the dangerous part: the next person to
// change one copy would read it and believe the other had followed. This cell
// exists so the claim is CHECKED — every action, against both paths, on the
// same account and the same grant.

// parityFixture builds an account whose role and grant are what the case
// asks for, and returns the session token and user id to drive both paths.
type parityCase struct {
	name      string
	role      string // account role
	grantRole string // "" = no grant at all
	disabled  bool
}

func TestAuthorizeParity_BothEntryPointsAgreeOnEveryAction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []parityCase{
		{"an admin with no grant", "admin", "", false},
		{"an admin with a reader grant", "admin", "reader", false},
		{"an editor with an editor grant", "editor", "editor", false},
		{"an editor with a reader grant", "editor", "reader", false},
		{"a reader with an editor grant", "reader", "editor", false},
		{"a reader with a reader grant", "reader", "reader", false},
		{"a reader with no grant", "reader", "", false},
		{"an editor whose account is disabled", "editor", "editor", true},
		{"an admin whose account is disabled", "admin", "admin", true},
	}
	actions := []Action{ActionRead, ActionWrite, ActionDDL, ActionManage}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, store, _ := newSvc(t)
			rootTok, root := mustBootstrap(t, s)

			connID, cerr := store.Connections.OnCtx(ctx).
				Set(meta.ConnName, "target").Set(meta.ConnEngine, "postgres").
				Set(meta.ConnDSNEnc, []byte{1}).Set(meta.ConnCreatedBy, root.UserID()).
				Set(meta.ConnCreatedAt, int64(1)).Set(meta.ConnUpdatedAt, int64(1)).Insert()
			if cerr != nil {
				t.Fatalf("connection: %v", cerr)
			}
			uid, err := s.CreateUser(ctx, rootTok, "subject", rootPass, c.role, testIP)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if c.grantRole != "" {
				if gerr := s.AddGrant(ctx, rootTok, uid, connID, c.grantRole, testIP); gerr != nil {
					t.Fatalf("AddGrant: %v", gerr)
				}
			}
			// The session token is minted BEFORE the account is disabled:
			// disabling must be caught by the decision path, not by the
			// absence of a token.
			userTok, _, lerr := s.Login(ctx, "subject", rootPass, testIP)
			if lerr != nil {
				t.Fatalf("Login: %v", lerr)
			}
			if c.disabled {
				if derr := s.SetUserDisabled(ctx, rootTok, uid, true, testIP); derr != nil {
					t.Fatalf("SetUserDisabled: %v", derr)
				}
			}

			for _, action := range actions {
				_, tokErr := s.Authorize(ctx, userTok, connID, action)
				userErr := s.AuthorizeUser(ctx, uid, connID, action)

				tokDenied := tokErr != nil
				userDenied := userErr != nil
				if tokDenied != userDenied {
					t.Errorf("%s: the token path %s and the user path %s — one rule, two answers, "+
						"which is the divergence a second copy of the policy was always going to "+
						"produce (token err %v; user err %v)",
						action, verdict(tokDenied), verdict(userDenied), tokErr, userErr)
					continue
				}
				// A denial must also be the SAME denial. ErrDenied and
				// ErrTokenInvalid mean different things to a caller, but a
				// disabled account denies on both paths and neither should
				// report a store failure.
				if tokDenied && !errors.Is(tokErr, ErrDenied) && !errors.Is(tokErr, ErrTokenInvalid) {
					t.Errorf("%s: token path denied with %v, which is neither ErrDenied nor "+
						"ErrTokenInvalid", action, tokErr)
				}
				if userDenied && !errors.Is(userErr, ErrDenied) {
					t.Errorf("%s: user path denied with %v, want ErrDenied", action, userErr)
				}
			}
		})
	}
}

func verdict(denied bool) string {
	if denied {
		return "DENIED"
	}
	return "ALLOWED"
}
