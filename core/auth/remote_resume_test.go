package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// detach marks rt's session detached until the far future, as a close write
// does.
func detach(t *testing.T, store *meta.Store, rt remoteToken) {
	t.Helper()
	if err := store.Sessions.OnCtx(context.Background()).With(meta.SessID, rt.session).
		Set(meta.SessAttachedConn, "").Set(meta.SessDetachedUntil, int64(9_999_999_998)).Update(); err != nil {
		t.Fatal(err)
	}
}

// A device, key or user revoked or disabled between the device proof and the
// resume: the session is not taken up, and it is not counted (the connection
// can still sign in, and be refused there).
func TestAResumeIsRefusedWhenItsDeviceKeyOrUserWentAway(t *testing.T) {
	s, store, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		revoke func(rt remoteToken)
	}{
		{"device revoked", func(rt remoteToken) {
			_ = store.RemoteDevices.OnCtx(ctx).With(meta.DevID, rt.device).Set(meta.DevRevokedAt, int64(5)).Update()
		}},
		{"SSH key revoked", func(rt remoteToken) {
			_ = store.SSHKeys.OnCtx(ctx).With(meta.SSHKeyID, rt.sshKey).Set(meta.SSHKeyRevokedAt, int64(5)).Update()
		}},
		{"user disabled", func(rt remoteToken) {
			_ = store.Users.OnCtx(ctx).With(meta.UserID, rt.userID).Set(meta.UserDisabled, int64(1)).Update()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			uid := root.userID
			if c.name == "user disabled" {
				if _, err := s.CreateUser(ctx, rootTok, "dora-"+c.name[:4], "dora-passphrase-long", meta.RoleReader, testIP); err != nil {
					t.Fatal(err)
				}
				u, err := store.Users.OnCtx(ctx).With(meta.UserName, "dora-"+c.name[:4]).Get()
				if err != nil {
					t.Fatal(err)
				}
				uid = u.ID
			}
			rt := seedRemoteToken(t, store, uid, c.name[:3], "conn-old")
			detach(t, store, rt)
			if _, err := s.ResumeRemote(ctx, RemoteResume{DeviceID: rt.device, ConnID: "conn-new"}, rt.token, testIP); err != nil {
				t.Fatalf("positive control: %v", err)
			}
			detach(t, store, rt)
			c.revoke(rt)
			_, err := s.ResumeRemote(ctx, RemoteResume{DeviceID: rt.device, ConnID: "conn-newer"}, rt.token, testIP)
			if !errors.Is(err, ErrRemoteResumeUnavailable) {
				t.Fatalf("%s: %v; want ErrRemoteResumeUnavailable", c.name, err)
			}
			if _, ok := DenialFor(err); ok {
				t.Fatalf("%s: the refusal is counted", c.name)
			}
		})
	}
}

// Two connections claiming one session at once: the one whose claim lands
// second loses (ErrRemoteResumeUnavailable) and the first keeps it. The
// competing claim is made between the read and the swap, where it lands on a
// store that runs both at once.
func TestAResumeThatLosesTheClaimTakesNothing(t *testing.T) {
	s, store, _ := newSvc(t)
	_, root := mustBootstrap(t, s)
	rt := seedRemoteToken(t, store, root.userID, "race", "conn-old")
	detach(t, store, rt)
	s.hookResumeClaim = func(tx *dao.Transaction) {
		if err := store.Sessions.On(tx).With(meta.SessID, rt.session).
			Set(meta.SessAttachedConn, "conn-winner").Set(meta.SessDetachedUntil, int64(0)).Update(); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.ResumeRemote(context.Background(), RemoteResume{DeviceID: rt.device, ConnID: "conn-loser"}, rt.token, testIP)
	if !errors.Is(err, ErrRemoteResumeUnavailable) {
		t.Fatalf("the losing claim: %v; want ErrRemoteResumeUnavailable", err)
	}
}
