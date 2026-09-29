package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

var (
	// ErrRemoteTokenForeign: a resume with a token that is not this device's
	// (unknown, or bound to another device). A counted denial.
	ErrRemoteTokenForeign = errors.New("auth: that session is not this device's")
	// ErrRemoteResumeUnavailable: the token is this device's, but its session
	// can no longer be taken up (the grace passed, it was revoked or expired,
	// or another connection took it first). Not counted: the client signs in
	// with the passphrase on the same connection.
	ErrRemoteResumeUnavailable = errors.New("auth: that session can no longer be resumed; sign in")
)

// RemoteResume is what a remote connection brings to remote.resume, all of
// it from the connection.
type RemoteResume struct {
	// DeviceID is the enrolled device the connection proved; required.
	DeviceID int64
	ConnID   string
}

// Resumed is a resume's result.
type Resumed struct {
	Identity  Identity
	SessionID int64
	// TookOver is the connection the session was taken from, when it was
	// still attached to an older connection of the same device ("" when it
	// was detached). The caller ends that connection.
	TookOver string
}

// ResumeRemote takes up a remote session on a new connection of the same
// device, without the passphrase: the reconnect within the grace.
//
// The session must be bound to rr.DeviceID (else ErrRemoteTokenForeign), live,
// and its device, SSH key and user unrevoked and enabled (else
// ErrRemoteResumeUnavailable). It is claimed by compare-and-swap on the value
// just read, in either of two states:
//   - detached, within its grace: the old connection's close write ran;
//   - still attached to another connection: the new one got here before the
//     old one's close write (a half-open TCP connection). The device has
//     just proved itself here, so the last attach wins.
//
// Losing the claim to another resume is ErrRemoteResumeUnavailable. In the
// same transaction the device's last-seen time and address are recorded
// (remote_new_ip for a new one). A late close write from the old connection
// matches no row afterwards, so it can never detach the new owner.
func (s *Service) ResumeRemote(ctx context.Context, rr RemoteResume, token, ip string) (Resumed, error) {
	if rr.DeviceID <= 0 || rr.ConnID == "" {
		return Resumed{}, ErrRemoteNotAttested
	}
	var out Resumed
	err := s.inTx(ctx, func(tx *dao.Transaction) error {
		sess, err := s.store.Sessions.On(tx).With(meta.SessTokenHash, tokenHash(token)).Get()
		if errors.Is(err, dao.ErrNoRows) {
			return ErrRemoteTokenForeign
		}
		if err != nil {
			return err
		}
		if sess.DeviceID != rr.DeviceID {
			return ErrRemoteTokenForeign
		}
		now := s.now().Unix()
		if sess.Revoked != 0 || now >= sess.ExpiresAt || sess.AttachedConn == rr.ConnID {
			return ErrRemoteResumeUnavailable
		}
		observed := sess.AttachedConn
		if observed == "" && sess.DetachedUntil <= now {
			return ErrRemoteResumeUnavailable // the grace passed
		}
		dev, err := s.store.RemoteDevices.On(tx).With(meta.DevID, sess.DeviceID).Get()
		if err != nil {
			return err
		}
		key, err := s.store.SSHKeys.On(tx).With(meta.SSHKeyID, dev.SSHKeyID).Get()
		if err != nil {
			return err
		}
		u, err := s.store.Users.On(tx).With(meta.UserID, sess.UserID).Get()
		if err != nil {
			return err
		}
		if dev.RevokedAt != 0 || key.RevokedAt != 0 || dev.UserID != u.ID || u.Disabled != 0 {
			return ErrRemoteResumeUnavailable
		}
		if s.hookResumeClaim != nil {
			s.hookResumeClaim(tx)
		}
		// The claim: a compare-and-swap on the state just read.
		conn := string(meta.SessAttachedConn)
		claimable := dao.And(dao.Eq(conn, ""), dao.Gt(string(meta.SessDetachedUntil), now))
		if observed != "" {
			claimable = dao.Eq(conn, observed)
		}
		if err := s.store.Sessions.On(tx).With(meta.SessID, sess.ID).WithPredicate(claimable).
			Set(meta.SessAttachedConn, rr.ConnID).Set(meta.SessDetachedUntil, int64(0)).Update(); err != nil {
			return err
		}
		after, err := s.store.Sessions.On(tx).With(meta.SessID, sess.ID).Get()
		if err != nil {
			return err
		}
		if after.AttachedConn != rr.ConnID {
			return ErrRemoteResumeUnavailable // another resume won
		}
		if err := s.store.RemoteDevices.On(tx).With(meta.DevID, dev.ID).Set(meta.DevLastSeenAt, now).Update(); err != nil {
			return err
		}
		if err := s.seenAtTx(tx, u, dev, ip, now); err != nil {
			return err
		}
		out = Resumed{Identity: Identity{userID: u.ID, name: u.Name, role: u.Role}, SessionID: sess.ID, TookOver: observed}
		return nil
	})
	if err != nil {
		return Resumed{}, err
	}
	return out, nil
}

// RotateDevice replaces the device key of the caller's device. The caller
// must be a remote connection signed in on that device (token resolves for
// caller). msg is remote.RotateMessage for the connection, the device, the
// current key and newPub; sigOld must be the current key's signature over
// it and sigNew newPub's. In one transaction the key is swapped, its age
// reset, and remote_device_enrolled written with detail "rotated". A proof
// that does not verify is ErrRemoteDeviceProofInvalid, and nothing changes.
func (s *Service) RotateDevice(ctx context.Context, token string, msgFor func(oldPub []byte) []byte, newPub, sigOld, sigNew []byte, ip string) (keyCreatedAt int64, err error) {
	caller := CallerFrom(ctx)
	if caller.Surface != SurfaceRemote || caller.DeviceID == 0 {
		return 0, ErrDenied
	}
	newText, newFP, err := DeviceKey(newPub)
	if err != nil {
		return 0, err
	}
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		ident, terr := s.resolveTokenTx(tx, caller, token)
		if terr != nil {
			return terr
		}
		dev, derr := s.store.RemoteDevices.On(tx).With(meta.DevID, caller.DeviceID).Get()
		if derr != nil {
			return derr
		}
		if dev.UserID != ident.userID || dev.RevokedAt != 0 {
			return ErrDenied
		}
		oldPub, perr := devicePubOf(dev.PublicKey)
		if perr != nil {
			return perr
		}
		if newText == dev.PublicKey {
			return fmt.Errorf("%w: the new key is the current one", ErrRemoteDeviceProofInvalid)
		}
		msg := msgFor(oldPub)
		if !ed25519.Verify(oldPub, msg, sigOld) || !ed25519.Verify(ed25519.PublicKey(newPub), msg, sigNew) {
			return ErrRemoteDeviceProofInvalid
		}
		now := s.now().Unix()
		if uerr := s.store.RemoteDevices.On(tx).With(meta.DevID, dev.ID).
			Set(meta.DevPublicKey, newText).Set(meta.DevFingerprint, newFP).
			Set(meta.DevKeyCreatedAt, now).Update(); uerr != nil {
			return uerr
		}
		keyCreatedAt = now
		return s.AuditTx(tx, ident.userID, ip, "remote_device_enrolled",
			fmt.Sprintf("rotated user=%s device=%s from=%s ip=%s", ident.name, newFP, dev.Fingerprint, ip))
	})
	if err != nil {
		return 0, err
	}
	return keyCreatedAt, nil
}

// devicePubOf is the raw ed25519 public key of a stored device key.
func devicePubOf(text string) (ed25519.PublicKey, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("auth: stored device key: %w", err)
	}
	ck, ok := k.(ssh.CryptoPublicKey)
	if !ok {
		return nil, errors.New("auth: stored device key has no crypto key")
	}
	pub, ok := ck.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("auth: stored device key is not ed25519")
	}
	return pub, nil
}
