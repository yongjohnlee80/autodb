package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// remoteToken is a session bound to a remote device: the SSH key and device
// rows it needs, and the session minted for it, attached to connection conn.
type remoteToken struct {
	token    string
	session  int64
	device   int64
	sshKey   int64
	conn     string
	userID   int64
	fpSuffix string
}

// seedRemoteToken inserts what a remote login leaves: an SSH key for userID,
// the device enrolled with it, and a live session bound to that device and
// attached to conn.
func seedRemoteToken(t *testing.T, store *meta.Store, userID int64, suffix, conn string) remoteToken {
	t.Helper()
	ctx := context.Background()
	key, err := store.SSHKeys.OnCtx(ctx).Set(meta.SSHKeyUserID, userID).
		Set(meta.SSHKeyPublicKey, "ssh-ed25519 AAAA"+suffix).Set(meta.SSHKeyFingerprint, "SHA256:key-"+suffix).
		Set(meta.SSHKeyAddedBy, userID).Set(meta.SSHKeyCreatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("seeding an SSH key: %v", err)
	}
	dev, err := store.RemoteDevices.OnCtx(ctx).Set(meta.DevSSHKeyID, key).Set(meta.DevUserID, userID).
		Set(meta.DevPublicKey, "dev-"+suffix).Set(meta.DevFingerprint, "SHA256:dev-"+suffix).
		Set(meta.DevEnrolledAt, int64(1)).Set(meta.DevEnrolledIP, "203.0.113.7").
		Set(meta.DevKeyCreatedAt, int64(1)).Set(meta.DevLastSeenAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("seeding a device: %v", err)
	}
	token := "remote-token-" + suffix
	sess, err := store.Sessions.OnCtx(ctx).
		Set(meta.SessTokenHash, tokenHash(token)).Set(meta.SessUserID, userID).
		Set(meta.SessIP, "203.0.113.7").Set(meta.SessCreatedAt, int64(1)).
		Set(meta.SessExpiresAt, int64(9_999_999_999)).Set(meta.SessRevoked, int64(0)).
		Set(meta.SessDeviceID, dev).Set(meta.SessAttachedConn, conn).Insert()
	if err != nil {
		t.Fatalf("seeding a remote session: %v", err)
	}
	return remoteToken{token: token, session: sess, device: dev, sshKey: key, conn: conn, userID: userID, fpSuffix: suffix}
}

// caller is the Caller of the connection that owns rt.
func (rt remoteToken) caller() Caller {
	return Caller{Surface: SurfaceRemote, DeviceID: rt.device, ConnID: rt.conn}
}

// A token works only on the surface it was minted for. A local token is
// refused on every remote connection. A remote token is refused locally, and
// on a remote connection unless that connection proved the token's device and
// owns the session.
func TestATokenWorksOnlyOnTheSurfaceItWasMintedFor(t *testing.T) {
	s, store, _ := newSvc(t)
	localTok, root := mustBootstrap(t, s)
	rt := seedRemoteToken(t, store, root.userID, "a", "conn-a")
	other := seedRemoteToken(t, store, root.userID, "b", "conn-b")
	// A detached session: its connection went away, so no connection owns it.
	detached := seedRemoteToken(t, store, root.userID, "c", "")
	ctx := context.Background()

	cases := []struct {
		name   string
		caller Caller
		token  string
		ok     bool
	}{
		{"local token, local caller", LocalCaller, localTok, true},
		{"local token, remote caller", rt.caller(), localTok, false},
		{"local token, remote caller with no device", Caller{Surface: SurfaceRemote, ConnID: "conn-a"}, localTok, false},
		{"remote token, owning connection", rt.caller(), rt.token, true},
		{"remote token, local caller", LocalCaller, rt.token, false},
		{"remote token, a connection that proved another device", Caller{Surface: SurfaceRemote, DeviceID: other.device, ConnID: "conn-a"}, rt.token, false},
		{"remote token, its device on a connection that does not own it", Caller{Surface: SurfaceRemote, DeviceID: rt.device, ConnID: "conn-z"}, rt.token, false},
		{"remote token, a connection that proved no device", Caller{Surface: SurfaceRemote, ConnID: "conn-a"}, rt.token, false},
		{"remote token, a connection with no id", Caller{Surface: SurfaceRemote, DeviceID: rt.device}, rt.token, false},
		{"detached remote token, its device on a connection with no id", Caller{Surface: SurfaceRemote, DeviceID: detached.device}, detached.token, false},
		{"detached remote token, its device on a connection with an id", Caller{Surface: SurfaceRemote, DeviceID: detached.device, ConnID: "conn-c"}, detached.token, false},
	}
	for _, c := range cases {
		_, _, err := s.resolveToken(ctx, c.caller, c.token)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: %v; want ErrTokenInvalid", c.name, err)
		}
		// The transaction path is the same check.
		terr := dao.RunTx(ctx, func(tx *dao.Transaction) error {
			_, err := s.resolveTokenTx(tx, c.caller, c.token)
			return err
		})
		if c.ok != (terr == nil) {
			t.Errorf("%s: on a transaction: %v; want ok=%v", c.name, terr, c.ok)
		}
	}
	// Through the public API, with the caller carried by the context.
	if _, err := s.ValidateToken(WithCaller(ctx, rt.caller()), rt.token); err != nil {
		t.Errorf("ValidateToken on the owning connection: %v", err)
	}
	if _, err := s.ValidateToken(ctx, rt.token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("ValidateToken of a remote token with no caller (local): %v; want ErrTokenInvalid", err)
	}
}

// Revoking the device, or the SSH key it was enrolled with, ends the remote
// token on its next resolution. A device row belonging to another user does
// not vouch for the session.
func TestARevokedDeviceOrKeyEndsTheRemoteToken(t *testing.T) {
	s, store, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()
	check := func(rt remoteToken) error {
		_, _, err := s.resolveToken(ctx, rt.caller(), rt.token)
		return err
	}

	dev := seedRemoteToken(t, store, root.userID, "d", "conn-d")
	if err := check(dev); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	if err := store.RemoteDevices.OnCtx(ctx).With(meta.DevID, dev.device).Set(meta.DevRevokedAt, int64(5)).Update(); err != nil {
		t.Fatal(err)
	}
	if err := check(dev); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("revoked device: %v; want ErrTokenInvalid", err)
	}

	key := seedRemoteToken(t, store, root.userID, "k", "conn-k")
	if err := store.SSHKeys.OnCtx(ctx).With(meta.SSHKeyID, key.sshKey).Set(meta.SSHKeyRevokedAt, int64(5)).Update(); err != nil {
		t.Fatal(err)
	}
	if err := check(key); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("revoked SSH key: %v; want ErrTokenInvalid", err)
	}

	if _, err := s.CreateUser(ctx, rootTok, "mallory", "mallory-passphrase", meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}
	mal, err := store.Users.OnCtx(ctx).With(meta.UserName, "mallory").Get()
	if err != nil {
		t.Fatal(err)
	}
	foreign := seedRemoteToken(t, store, root.userID, "f", "conn-f")
	if err := store.RemoteDevices.OnCtx(ctx).With(meta.DevID, foreign.device).Set(meta.DevUserID, mal.ID).Update(); err != nil {
		t.Fatal(err)
	}
	if err := check(foreign); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a device of another user: %v; want ErrTokenInvalid", err)
	}
}

// Inside an open transaction the device is read on that transaction: revoked
// there, the token is refused there, before anything commits.
func TestARevocationInsideTheTransactionIsSeenThere(t *testing.T) {
	s, store, _ := newSvc(t)
	_, root := mustBootstrap(t, s)
	rt := seedRemoteToken(t, store, root.userID, "t", "conn-t")
	err := dao.RunTx(context.Background(), func(tx *dao.Transaction) error {
		if _, err := s.resolveTokenTx(tx, rt.caller(), rt.token); err != nil {
			t.Fatalf("positive control on the transaction: %v", err)
		}
		if err := store.RemoteDevices.On(tx).With(meta.DevID, rt.device).Set(meta.DevRevokedAt, int64(5)).Update(); err != nil {
			return err
		}
		_, err := s.resolveTokenTx(tx, rt.caller(), rt.token)
		return err
	})
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a device revoked inside the transaction: %v; want ErrTokenInvalid", err)
	}
}

// A transaction-writing verb with a remote token presented from a connection
// that proved a different device, or after the device was revoked, is refused
// and writes nothing: a PAT create, a passphrase change, a creator grant.
func TestWritingVerbsRefuseAMismatchedOrRevokedDevice(t *testing.T) {
	s, store, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, root.userID)
	rt := seedRemoteToken(t, store, root.userID, "w", "conn-w")
	other := seedRemoteToken(t, store, root.userID, "x", "conn-x")
	wrongDevice := WithCaller(ctx, Caller{Surface: SurfaceRemote, DeviceID: other.device, ConnID: rt.conn})
	owning := WithCaller(ctx, rt.caller())

	pats := func() uint64 {
		n, err := store.PATs.OnCtx(ctx).Count()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Positive control: the owning connection can mint.
	if _, err := s.CreatePAT(owning, rt.token, "remote-ok", conn, 0, nil, false, nil, testIP); err != nil {
		t.Fatalf("positive control: the owning connection could not mint a PAT: %v", err)
	}
	before := pats()
	if _, err := s.CreatePAT(wrongDevice, rt.token, "remote-wrong", conn, 0, nil, false, nil, testIP); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("PAT create from another device: %v; want ErrTokenInvalid", err)
	}
	if err := s.ChangePassphrase(wrongDevice, rt.token, rootPass, "a-new-root-passphrase", testIP); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("passphrase change from another device: %v; want ErrTokenInvalid", err)
	}
	gerr := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		_, err := s.GrantCreatorTx(tx, Caller{Surface: SurfaceRemote, DeviceID: other.device, ConnID: rt.conn}, rt.token, conn)
		return err
	})
	if !errors.Is(gerr, ErrTokenInvalid) {
		t.Errorf("creator grant from another device: %v; want ErrTokenInvalid", gerr)
	}

	if err := store.RemoteDevices.OnCtx(ctx).With(meta.DevID, rt.device).Set(meta.DevRevokedAt, int64(5)).Update(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePAT(owning, rt.token, "remote-revoked", conn, 0, nil, false, nil, testIP); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("PAT create after the device was revoked: %v; want ErrTokenInvalid", err)
	}
	if err := s.ChangePassphrase(owning, rt.token, rootPass, "a-new-root-passphrase", testIP); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("passphrase change after the device was revoked: %v; want ErrTokenInvalid", err)
	}

	if n := pats(); n != before {
		t.Errorf("PATs %d after the refusals, want %d", n, before)
	}
	if _, _, err := s.Login(ctx, "root", rootPass, testIP); err != nil {
		t.Errorf("the passphrase changed: the old one no longer signs in: %v", err)
	}
	_ = rootTok
}

// Absent a caller, a context is local; WithCaller carries one.
func TestCallerFromDefaultsToLocal(t *testing.T) {
	ctx := context.Background()
	if c := CallerFrom(ctx); c != LocalCaller {
		t.Fatalf("no caller: %+v; want LocalCaller", c)
	}
	want := Caller{Surface: SurfaceRemote, DeviceID: 3, ConnID: "c"}
	if c := CallerFrom(WithCaller(ctx, want)); c != want {
		t.Fatalf("carried caller %+v; want %+v", c, want)
	}
}
