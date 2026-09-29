package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The refusals of a remote device proof and sign-in. Each is a counted
// denial (DenialFor names its reason): the connection that earned one is
// ended.
var (
	// ErrRemoteDeviceProofInvalid: the device's signature does not verify.
	ErrRemoteDeviceProofInvalid = errors.New("auth: the device proof is not valid")
	// ErrRemoteDeviceMismatch: the SSH key has enrolled a different device,
	// or another connection enrolled one for it first.
	ErrRemoteDeviceMismatch = errors.New("auth: this SSH key is enrolled from another device")
	// ErrRemoteDeviceRevoked: the device proved is one that was revoked.
	ErrRemoteDeviceRevoked = errors.New("auth: this device was revoked")
	// ErrRemoteUserMismatch: the sign-in names someone other than the SSH
	// key's owner.
	ErrRemoteUserMismatch = errors.New("auth: the sign-in is not the SSH key's owner")
	// ErrRemoteNotAttested: a sign-in on a connection that proved no device.
	ErrRemoteNotAttested = errors.New("auth: prove the device before signing in")
)

// DenialFor is the denial reason of a refused remote proof or sign-in, and
// whether err is one. A wrong passphrase or an unknown name is login_failed.
func DenialFor(err error) (DenialReason, bool) {
	switch {
	case errors.Is(err, ErrRemoteDeviceProofInvalid):
		return DenialDeviceProofInvalid, true
	case errors.Is(err, ErrRemoteDeviceMismatch):
		return DenialDeviceMismatch, true
	case errors.Is(err, ErrRemoteDeviceRevoked):
		return DenialDeviceRevoked, true
	case errors.Is(err, ErrRemoteUserMismatch):
		return DenialLoginUserMismatch, true
	case errors.Is(err, ErrRemoteNotAttested), errors.Is(err, ErrRemoteTokenForeign):
		return DenialProtocolViolation, true
	case errors.Is(err, ErrBadCredentials), errors.Is(err, ErrDenied), errors.Is(err, ErrNoKeyslot):
		return DenialLoginFailed, true
	}
	return "", false
}

// DeviceKey is the canonical text of an ed25519 device public key, as the
// store keeps it (authorized-key form), and its fingerprint.
func DeviceKey(pub []byte) (text, fingerprint string, err error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", "", fmt.Errorf("%w: a device key is %d bytes, got %d", ErrRemoteDeviceProofInvalid, ed25519.PublicKeySize, len(pub))
	}
	k, err := ssh.NewPublicKey(ed25519.PublicKey(pub))
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrRemoteDeviceProofInvalid, err)
	}
	return string(ssh.MarshalAuthorizedKey(k)), ssh.FingerprintSHA256(k), nil
}

// Attestation is what a device proof established.
type Attestation struct {
	// DeviceID is the SSH key's enrolled device the proof matched; 0 when
	// the key has no live device and the proof is of a device to enroll.
	DeviceID int64
	// KeyCreatedAt is when that device's key was made (what rotation ages).
	KeyCreatedAt int64
}

// AttestRemote checks a device proof for SSH key keyID: sig must be pub's
// ed25519 signature over msg (remote.AttestMessage for this connection).
//
//   - The key has a live device: pub must be it, else ErrRemoteDeviceMismatch.
//   - The key has none: pub must not be a device of this key that was
//     revoked (ErrRemoteDeviceRevoked); otherwise it is to be enrolled by
//     the sign-in, and DeviceID is 0.
//
// Nothing is written. The proof comes before the passphrase is sent, so a
// copy of the SSH key on another machine is refused without it.
func (s *Service) AttestRemote(ctx context.Context, keyID int64, msg, pub, sig []byte) (Attestation, error) {
	text, _, err := DeviceKey(pub)
	if err != nil {
		return Attestation{}, err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return Attestation{}, ErrRemoteDeviceProofInvalid
	}
	devs, err := s.store.RemoteDevices.OnCtx(ctx).With(meta.DevSSHKeyID, keyID).Select()
	if err != nil {
		return Attestation{}, err
	}
	revoked := false
	for _, d := range devs {
		if d.RevokedAt == 0 {
			if d.PublicKey != text {
				return Attestation{}, ErrRemoteDeviceMismatch
			}
			return Attestation{DeviceID: d.ID, KeyCreatedAt: d.KeyCreatedAt}, nil
		}
		if d.PublicKey == text {
			revoked = true
		}
	}
	if revoked {
		return Attestation{}, ErrRemoteDeviceRevoked
	}
	return Attestation{}, nil
}

// RemoteLogin is what a remote connection brings to its sign-in, all of it
// from the connection (remote.Peer), none from the client.
type RemoteLogin struct {
	// SSHKeyID and OwnerID are the SSH key the connection authenticated
	// with and its owner.
	SSHKeyID, OwnerID int64
	SSHKeyFP          string
	// DeviceID is the enrolled device the connection proved; PendingPub the
	// device key it proved that is to be enrolled. Exactly one is set.
	DeviceID   int64
	PendingPub []byte
	// ConnID is the connection's id: the session is bound to it.
	ConnID string
	// ClientVersion is what the client said it is, for the enrollment event.
	ClientVersion string
}

// RemoteSignIn is a remote sign-in's result.
type RemoteSignIn struct {
	Token     string
	Identity  Identity
	SessionID int64
	// DeviceID is the device the session is bound to: the one proved, or
	// the one this sign-in enrolled.
	DeviceID int64
	Enrolled bool
}

// LoginRemote is auth.login on a remote connection. It is Login with three
// differences:
//   - the global IP allowlist does not gate it: the SSH key and the device
//     are the gate, and signing in remotely is how a new address is added;
//   - the name must be the SSH key's owner (ErrRemoteUserMismatch);
//   - the session is bound to the device and to the connection, and the
//     sign-in's remote bookkeeping commits with it in one transaction:
//     the device's enrollment (the one-live-device-per-key index decides a
//     race, and the loser is ErrRemoteDeviceMismatch), its last-seen time,
//     the SSH key's last use, the address, and the events
//     (remote_device_enrolled; remote_new_ip for a known device at a new
//     address).
//
// Everything else (argon2id, the keyslot, the disabled check, the audit row)
// is Login's. ip is the connection's real address.
func (s *Service) LoginRemote(ctx context.Context, rl RemoteLogin, name, passphrase, ip string) (RemoteSignIn, error) {
	if (rl.DeviceID == 0) == (rl.PendingPub == nil) {
		return RemoteSignIn{}, ErrRemoteNotAttested
	}
	var out RemoteSignIn
	token, ident, err := s.login(ctx, name, passphrase, ip, "", &remoteSession{rl: rl, out: &out})
	if err != nil {
		return RemoteSignIn{}, err
	}
	out.Token, out.Identity = token, ident
	return out, nil
}

// remoteSession is the remote half of a sign-in, carried through login.
type remoteSession struct {
	rl  RemoteLogin
	out *RemoteSignIn
}

// commitTx is the remote sign-in's work inside login's transaction: it
// re-checks the SSH key, enrolls or re-checks the device, records the
// address, and mints the device-bound session.
func (r *remoteSession) commitTx(s *Service, tx *dao.Transaction, u *meta.User, ip string) (string, error) {
	now := s.now().Unix()
	key, err := s.store.SSHKeys.On(tx).With(meta.SSHKeyID, r.rl.SSHKeyID).Get()
	if errors.Is(err, dao.ErrNoRows) || (err == nil && (key.RevokedAt != 0 || key.UserID != u.ID)) {
		return "", ErrBadCredentials
	}
	if err != nil {
		return "", err
	}
	deviceID := r.rl.DeviceID
	if deviceID == 0 {
		text, fp, derr := DeviceKey(r.rl.PendingPub)
		if derr != nil {
			return "", derr
		}
		live, cerr := s.store.RemoteDevices.On(tx).With(meta.DevSSHKeyID, key.ID).With(meta.DevRevokedAt, int64(0)).Exists()
		if cerr != nil {
			return "", cerr
		}
		if live {
			return "", ErrRemoteDeviceMismatch
		}
		deviceID, err = s.store.RemoteDevices.On(tx).Set(meta.DevSSHKeyID, key.ID).Set(meta.DevUserID, u.ID).
			Set(meta.DevPublicKey, text).Set(meta.DevFingerprint, fp).
			Set(meta.DevEnrolledAt, now).Set(meta.DevEnrolledIP, ip).
			Set(meta.DevKeyCreatedAt, now).Set(meta.DevLastSeenAt, now).
			Set(meta.DevRevokedAt, int64(0)).Set(meta.DevRevokedBy, int64(0)).Insert()
		if err != nil {
			// The one-live-device-per-key index: another connection
			// enrolled a device for this key first.
			return "", fmt.Errorf("%w: %v", ErrRemoteDeviceMismatch, err)
		}
		if _, err := s.store.RemoteDeviceIPs.On(tx).Set(meta.DevIPDeviceID, deviceID).Set(meta.DevIPIP, ip).
			Set(meta.DevIPFirstSeenAt, now).Set(meta.DevIPLastSeenAt, now).Insert(); err != nil {
			return "", err
		}
		if err := s.AuditTx(tx, u.ID, ip, "remote_device_enrolled", fmt.Sprintf("user=%s key=%s device=%s ip=%s client=%s",
			u.Name, key.Fingerprint, fp, ip, r.rl.ClientVersion)); err != nil {
			return "", err
		}
		r.out.Enrolled = true
	} else {
		dev, derr := s.store.RemoteDevices.On(tx).With(meta.DevID, deviceID).Get()
		if errors.Is(derr, dao.ErrNoRows) || (derr == nil && (dev.RevokedAt != 0 || dev.SSHKeyID != key.ID || dev.UserID != u.ID)) {
			return "", ErrRemoteDeviceRevoked
		}
		if derr != nil {
			return "", derr
		}
		if err := s.store.RemoteDevices.On(tx).With(meta.DevID, deviceID).Set(meta.DevLastSeenAt, now).Update(); err != nil {
			return "", err
		}
		if err := s.seenAtTx(tx, u, dev, ip, now); err != nil {
			return "", err
		}
	}
	if err := s.store.SSHKeys.On(tx).With(meta.SSHKeyID, key.ID).Set(meta.SSHKeyLastUsedAt, now).Update(); err != nil {
		return "", err
	}
	token, sessID, err := s.newBoundSessionTx(tx, u.ID, ip, deviceID, r.rl.ConnID)
	if err != nil {
		return "", err
	}
	r.out.SessionID, r.out.DeviceID = sessID, deviceID
	return token, nil
}

// seenAtTx records that enrolled device dev connected from ip: a new address
// is inserted and written as remote_new_ip, with the address it was last
// seen from; a known one has its last-seen time moved.
func (s *Service) seenAtTx(tx *dao.Transaction, u *meta.User, dev *meta.RemoteDevice, ip string, now int64) error {
	ips, err := s.store.RemoteDeviceIPs.On(tx).With(meta.DevIPDeviceID, dev.ID).Select()
	if err != nil {
		return err
	}
	var prev *meta.RemoteDeviceIP
	for _, row := range ips {
		if row.IP == ip {
			return s.store.RemoteDeviceIPs.On(tx).With(meta.DevIPID, row.ID).Set(meta.DevIPLastSeenAt, now).Update()
		}
		if prev == nil || row.LastSeenAt > prev.LastSeenAt {
			prev = row
		}
	}
	if _, err := s.store.RemoteDeviceIPs.On(tx).Set(meta.DevIPDeviceID, dev.ID).Set(meta.DevIPIP, ip).
		Set(meta.DevIPFirstSeenAt, now).Set(meta.DevIPLastSeenAt, now).Insert(); err != nil {
		return err
	}
	last := ""
	if prev != nil {
		last = prev.IP
	}
	return s.AuditTx(tx, u.ID, ip, "remote_new_ip", fmt.Sprintf("user=%s device=%s ip=%s previous=%s",
		u.Name, dev.Fingerprint, ip, last))
}

// DetachRemoteSession is the close write of a remote connection that ended
// without signing out: session, if connection connID still owns it, is
// detached and may be taken up again by its device until grace has passed.
// Then any other session still attached to connID is revoked: there should
// be none (a connection signs in once), and extra reports how many there
// were. It runs on a fresh bounded context, never the request's.
func (s *Service) DetachRemoteSession(session int64, connID string, grace time.Duration) (extra uint64, err error) {
	if connID == "" {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		if session > 0 {
			if err := s.store.Sessions.On(tx).With(meta.SessID, session).With(meta.SessAttachedConn, connID).
				Set(meta.SessAttachedConn, "").Set(meta.SessDetachedUntil, s.now().Add(grace).Unix()).Update(); err != nil {
				return err
			}
		}
		left := func() dao.DAO[*meta.Session, meta.SessionField, int64] {
			return s.store.Sessions.On(tx).With(meta.SessAttachedConn, connID).With(meta.SessRevoked, int64(0))
		}
		var cerr error
		if extra, cerr = left().Count(); cerr != nil || extra == 0 {
			return cerr
		}
		return left().Set(meta.SessRevoked, int64(1)).Update()
	})
	return extra, err
}
