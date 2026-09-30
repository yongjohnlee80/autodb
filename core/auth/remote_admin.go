package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Account administration of remote access: the SSH keys on a profile, the
// devices they enrolled, and the blocked addresses. Each change is an
// account change, audited like the rest of the user_* family; it is not one
// of the three connection events (remote_device_enrolled, remote_new_ip,
// remote_access_denied).
//
// Who may act: a user on their own keys and devices, an admin on anyone's.
// The blocked addresses are admins' only.

var (
	// ErrSSHKeyInvalid: the text is not one SSH public key autodb accepts.
	ErrSSHKeyInvalid = errors.New("auth: not an SSH public key autodb accepts")
	// ErrSSHKeyTaken: a live key with that fingerprint is registered already
	// (on any profile: a key names exactly one account).
	ErrSSHKeyTaken = errors.New("auth: that SSH key is already registered")
	// ErrNotFound: no such key or device, or not the caller's to see.
	ErrNotFound = errors.New("auth: no such key or device")
)

// minRSABits is the smallest RSA key accepted.
const minRSABits = 3072

// SSHKeyInfo is one registered SSH key, with the device it enrolled.
type SSHKeyInfo struct {
	ID          int64
	UserID      int64
	User        string
	Label       string
	Fingerprint string
	Type        string
	AddedBy     int64
	CreatedAt   int64
	LastUsedAt  int64
	// Device is the key's live device, nil until its first connect.
	Device *DeviceInfo
}

// DeviceInfo is one enrolled device.
type DeviceInfo struct {
	ID           int64
	SSHKeyID     int64
	UserID       int64
	User         string
	Fingerprint  string
	EnrolledAt   int64
	EnrolledIP   string
	KeyCreatedAt int64
	LastSeenAt   int64
	LastIP       string
	RevokedAt    int64
}

// BlockInfo is one address prefix with refusals counted against it.
type BlockInfo struct {
	Prefix        string
	Failures      int64
	LastFailureAt int64
	// BlockedUntil is when the block ends; 0 or past when not blocked.
	BlockedUntil int64
	// Reason is the latest refusal's reason.
	Reason string
}

// targetUser resolves who an account action is about: userID, or the
// caller when userID is 0. It refuses (ErrDenied) a caller acting on
// someone else without being an admin.
func (s *Service) targetUser(ctx context.Context, token string, userID int64) (actor Identity, target *meta.User, err error) {
	actor, _, err = s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return Identity{}, nil, err
	}
	if userID == 0 {
		userID = actor.userID
	}
	if userID != actor.userID && actor.role != meta.RoleAdmin {
		return Identity{}, nil, ErrDenied
	}
	target, err = s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return Identity{}, nil, ErrNotFound
	}
	if err != nil {
		return Identity{}, nil, err
	}
	return actor, target, nil
}

// ParseSSHKey reads one SSH public key in authorized-key form and returns
// its canonical text, fingerprint and type. DSA and RSA under 3072 bits are
// refused; so is a key with options (from=, command=): autodb reads only the
// key.
func ParseSSHKey(text string) (canonical, fingerprint, keyType string, err error) {
	k, _, options, rest, perr := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(text)))
	if perr != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrSSHKeyInvalid, perr)
	}
	if len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return "", "", "", fmt.Errorf("%w: one key, with no options", ErrSSHKeyInvalid)
	}
	switch k.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		var pub *rsa.PublicKey
		if ck, ok := k.(ssh.CryptoPublicKey); ok {
			pub, _ = ck.CryptoPublicKey().(*rsa.PublicKey)
		}
		if pub == nil || pub.N.BitLen() < minRSABits {
			return "", "", "", fmt.Errorf("%w: an RSA key must be at least %d bits", ErrSSHKeyInvalid, minRSABits)
		}
	default:
		return "", "", "", fmt.Errorf("%w: %s keys are not accepted", ErrSSHKeyInvalid, k.Type())
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), ssh.FingerprintSHA256(k), k.Type(), nil
}

// AddSSHKey registers publicKey on userID's profile (0: the caller's). A user
// adding a key to their own profile re-enters their passphrase, checked
// here, so a session left open cannot quietly add one; an admin adding a key
// to someone else's profile does not. The key has no device until its first
// connect. Audited as user_ssh_key_added.
func (s *Service) AddSSHKey(ctx context.Context, token string, userID int64, publicKey, label, passphrase, ip string) (SSHKeyInfo, error) {
	actor, target, err := s.targetUser(ctx, token, userID)
	if err != nil {
		return SSHKeyInfo{}, err
	}
	if target.ID == actor.userID && !s.passphraseMatches(target, passphrase) {
		if aerr := s.Audit(ctx, actor.userID, ip, "login_failed", target.Name+" (adding an SSH key)"); aerr != nil {
			return SSHKeyInfo{}, aerr
		}
		return SSHKeyInfo{}, ErrBadCredentials
	}
	canonical, fp, keyType, err := ParseSSHKey(publicKey)
	if err != nil {
		return SSHKeyInfo{}, err
	}
	label = strings.TrimSpace(label)
	var id int64
	now := s.now().Unix()
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		taken, terr := s.store.SSHKeys.On(tx).With(meta.SSHKeyFingerprint, fp).With(meta.SSHKeyRevokedAt, int64(0)).Exists()
		if terr != nil {
			return terr
		}
		if taken {
			return ErrSSHKeyTaken
		}
		if id, terr = s.store.SSHKeys.On(tx).Set(meta.SSHKeyUserID, target.ID).Set(meta.SSHKeyLabel, label).
			Set(meta.SSHKeyPublicKey, canonical).Set(meta.SSHKeyFingerprint, fp).
			Set(meta.SSHKeyAddedBy, actor.userID).Set(meta.SSHKeyCreatedAt, now).
			Set(meta.SSHKeyLastUsedAt, int64(0)).Set(meta.SSHKeyRevokedAt, int64(0)).
			Set(meta.SSHKeyRevokedBy, int64(0)).Insert(); terr != nil {
			return terr
		}
		return s.AuditTx(tx, actor.userID, ip, "user_ssh_key_added",
			fmt.Sprintf("user=%s key=%s type=%s label=%q", target.Name, fp, keyType, label))
	})
	if err != nil {
		return SSHKeyInfo{}, err
	}
	return SSHKeyInfo{ID: id, UserID: target.ID, User: target.Name, Label: label, Fingerprint: fp,
		Type: keyType, AddedBy: actor.userID, CreatedAt: now}, nil
}

// passphraseMatches checks passphrase against u's stored verifier, at the
// cost of a sign-in.
func (s *Service) passphraseMatches(u *meta.User, passphrase string) bool {
	params, verifier, err := decodeHash(string(u.PassHash))
	if err != nil {
		dummyDerive(passphrase)
		return false
	}
	_, authHalf := deriveKeys(passphrase, params)
	return verifyAuthHalf(authHalf, verifier)
}

// ListSSHKeys lists the live SSH keys on userID's profile (0: the caller's),
// each with its live device. userID -1 lists every user's, for an admin.
func (s *Service) ListSSHKeys(ctx context.Context, token string, userID int64) ([]SSHKeyInfo, error) {
	q, names, err := s.remoteListScope(ctx, token, userID)
	if err != nil {
		return nil, err
	}
	keys, err := s.store.SSHKeys.OnCtx(ctx).With(meta.SSHKeyRevokedAt, int64(0)).WithPredicate(q(string(meta.SSHKeyUserID))).Select()
	if err != nil {
		return nil, err
	}
	devs, err := s.listDevices(ctx, q, false)
	if err != nil {
		return nil, err
	}
	byKey := map[int64]*DeviceInfo{}
	for i := range devs {
		byKey[devs[i].SSHKeyID] = &devs[i]
	}
	out := make([]SSHKeyInfo, 0, len(keys))
	for _, k := range keys {
		_, _, typ, _ := ParseSSHKey(k.PublicKey)
		out = append(out, SSHKeyInfo{ID: k.ID, UserID: k.UserID, User: names(k.UserID), Label: k.Label,
			Fingerprint: k.Fingerprint, Type: typ, AddedBy: k.AddedBy, CreatedAt: k.CreatedAt,
			LastUsedAt: k.LastUsedAt, Device: byKey[k.ID]})
	}
	return out, nil
}

// ListDevices lists the devices of userID (0: the caller's; -1: everyone's,
// for an admin), revoked ones too when withRevoked.
func (s *Service) ListDevices(ctx context.Context, token string, userID int64, withRevoked bool) ([]DeviceInfo, error) {
	q, _, err := s.remoteListScope(ctx, token, userID)
	if err != nil {
		return nil, err
	}
	return s.listDevices(ctx, q, withRevoked)
}

// remoteListScope is a listing's user predicate and a user-name lookup.
func (s *Service) remoteListScope(ctx context.Context, token string, userID int64) (func(col string) dao.Predicate, func(int64) string, error) {
	all := userID == -1
	if all {
		userID = 0
	}
	actor, target, err := s.targetUser(ctx, token, userID)
	if err != nil {
		return nil, nil, err
	}
	if all && actor.role != meta.RoleAdmin {
		return nil, nil, ErrDenied
	}
	names := map[int64]string{}
	name := func(id int64) string {
		if n, ok := names[id]; ok {
			return n
		}
		n := ""
		if u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, id).Get(); err == nil {
			n = u.Name
		}
		names[id] = n
		return n
	}
	q := func(col string) dao.Predicate {
		if all {
			return dao.Gt(col, int64(0))
		}
		return dao.Eq(col, target.ID)
	}
	return q, name, nil
}

func (s *Service) listDevices(ctx context.Context, q func(col string) dao.Predicate, withRevoked bool) ([]DeviceInfo, error) {
	dq := s.store.RemoteDevices.OnCtx(ctx).WithPredicate(q(string(meta.DevUserID)))
	if !withRevoked {
		dq = dq.With(meta.DevRevokedAt, int64(0))
	}
	devs, err := dq.Select()
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		if _, ok := names[d.UserID]; !ok {
			if u, uerr := s.store.Users.OnCtx(ctx).With(meta.UserID, d.UserID).Get(); uerr == nil {
				names[d.UserID] = u.Name
			}
		}
		info := DeviceInfo{ID: d.ID, SSHKeyID: d.SSHKeyID, UserID: d.UserID, User: names[d.UserID],
			Fingerprint: d.Fingerprint, EnrolledAt: d.EnrolledAt, EnrolledIP: d.EnrolledIP,
			KeyCreatedAt: d.KeyCreatedAt, LastSeenAt: d.LastSeenAt, RevokedAt: d.RevokedAt}
		ips, ierr := s.store.RemoteDeviceIPs.OnCtx(ctx).With(meta.DevIPDeviceID, d.ID).Select()
		if ierr != nil {
			return nil, ierr
		}
		var last int64
		for _, ip := range ips {
			if ip.LastSeenAt >= last {
				last, info.LastIP = ip.LastSeenAt, ip.IP
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// LabelSSHKey renames a live key: its owner's, or any for an admin.
func (s *Service) LabelSSHKey(ctx context.Context, token string, keyID int64, label, ip string) error {
	actor, key, target, err := s.ownKey(ctx, token, keyID)
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.store.SSHKeys.On(tx).With(meta.SSHKeyID, key.ID).Set(meta.SSHKeyLabel, label).Update(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "user_ssh_key_labeled",
			fmt.Sprintf("user=%s key=%s label=%q", target.Name, key.Fingerprint, label))
	})
}

// ownKey resolves a live key the caller may act on: their own, or any for an
// admin. Another user's key is ErrNotFound to a non-admin, as a missing one
// is.
func (s *Service) ownKey(ctx context.Context, token string, keyID int64) (Identity, *meta.UserSSHKey, *meta.User, error) {
	actor, _, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return Identity{}, nil, nil, err
	}
	key, err := s.store.SSHKeys.OnCtx(ctx).With(meta.SSHKeyID, keyID).With(meta.SSHKeyRevokedAt, int64(0)).Get()
	if errors.Is(err, dao.ErrNoRows) || (err == nil && key.UserID != actor.userID && actor.role != meta.RoleAdmin) {
		return Identity{}, nil, nil, ErrNotFound
	}
	if err != nil {
		return Identity{}, nil, nil, err
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, key.UserID).Get()
	if err != nil {
		return Identity{}, nil, nil, err
	}
	return actor, key, target, nil
}

// RevokeSSHKey revokes a key (its owner's, or any for an admin), and with it
// its device and every session bound to that device, in one transaction.
// The caller then ends the key's live connections (remote.BySSHKey). Audited
// as user_ssh_key_revoked.
func (s *Service) RevokeSSHKey(ctx context.Context, token string, keyID int64, ip string) error {
	actor, key, target, err := s.ownKey(ctx, token, keyID)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		now := s.now().Unix()
		if err := s.store.SSHKeys.On(tx).With(meta.SSHKeyID, key.ID).
			Set(meta.SSHKeyRevokedAt, now).Set(meta.SSHKeyRevokedBy, actor.userID).Update(); err != nil {
			return err
		}
		devs, err := s.store.RemoteDevices.On(tx).With(meta.DevSSHKeyID, key.ID).With(meta.DevRevokedAt, int64(0)).Select()
		if err != nil {
			return err
		}
		for _, d := range devs {
			if err := s.revokeDeviceTx(tx, d.ID, actor.userID, now); err != nil {
				return err
			}
		}
		return s.AuditTx(tx, actor.userID, ip, "user_ssh_key_revoked",
			fmt.Sprintf("user=%s key=%s", target.Name, key.Fingerprint))
	})
}

// RevokeDevice revokes a device (its owner's, or any for an admin) and every
// session bound to it. Its SSH key must enroll afresh: this is also how a
// wiped machine is let back in. The caller then ends the device's live
// connections (remote.ByDevice). Audited as user_remote_device_revoked.
func (s *Service) RevokeDevice(ctx context.Context, token string, deviceID int64, ip string) error {
	actor, _, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return err
	}
	dev, err := s.store.RemoteDevices.OnCtx(ctx).With(meta.DevID, deviceID).With(meta.DevRevokedAt, int64(0)).Get()
	if errors.Is(err, dao.ErrNoRows) || (err == nil && dev.UserID != actor.userID && actor.role != meta.RoleAdmin) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, dev.UserID).Get()
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.revokeDeviceTx(tx, dev.ID, actor.userID, s.now().Unix()); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "user_remote_device_revoked",
			fmt.Sprintf("user=%s device=%s", target.Name, dev.Fingerprint))
	})
}

// revokeDeviceTx revokes device id and the sessions bound to it.
func (s *Service) revokeDeviceTx(tx *dao.Transaction, id, by, now int64) error {
	if err := s.store.RemoteDevices.On(tx).With(meta.DevID, id).
		Set(meta.DevRevokedAt, now).Set(meta.DevRevokedBy, by).Update(); err != nil {
		return err
	}
	return s.store.Sessions.On(tx).With(meta.SessDeviceID, id).With(meta.SessRevoked, int64(0)).
		Set(meta.SessRevoked, int64(1)).Update()
}

// ListBlocks lists, for an admin, the address prefixes with refusals
// counted against them, each with its latest refusal's reason. Whether one
// is blocked now is BlockedUntil against the clock.
func (s *Service) ListBlocks(ctx context.Context, token string) ([]BlockInfo, error) {
	if _, err := s.requireAdmin(ctx, token); err != nil {
		return nil, err
	}
	rows, err := s.store.RemoteIPBlocks.OnCtx(ctx).WithPredicate(dao.Or(
		dao.Gt(string(meta.BlockFailures), int64(0)),
		dao.Gt(string(meta.BlockUntil), s.now().Unix()))).Select()
	if err != nil {
		return nil, err
	}
	out := make([]BlockInfo, 0, len(rows))
	for _, b := range rows {
		info := BlockInfo{Prefix: b.Prefix, Failures: b.ConsecutiveFailures, LastFailureAt: b.LastFailureAt, BlockedUntil: b.BlockedUntil}
		evs, eerr := s.store.RemoteDenials.OnCtx(ctx).With(meta.DenialPrefix, b.Prefix).Select()
		if eerr != nil {
			return nil, eerr
		}
		var at int64
		for _, e := range evs {
			if e.OccurredAt >= at {
				at, info.Reason = e.OccurredAt, e.Reason
			}
		}
		out = append(out, info)
	}
	return out, nil
}
