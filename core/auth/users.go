package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// NeedsBootstrap reports whether no users exist yet — both frontends must
// then prompt for the root passphrase (Objective 10).
func (s *Service) NeedsBootstrap(ctx context.Context) (bool, error) {
	n, err := s.store.Users.OnCtx(ctx).Count()
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// Bootstrap creates the install master key and the root admin, and unlocks
// the service. ErrBootstrapDone when users already exist.
func (s *Service) Bootstrap(ctx context.Context, name, passphrase, ip string) (Identity, error) {
	needs, err := s.NeedsBootstrap(ctx)
	if err != nil {
		return Identity{}, err
	}
	if !needs {
		return Identity{}, ErrBootstrapDone
	}
	mk, err := newKey()
	if err != nil {
		return Identity{}, err
	}
	id, err := s.insertUser(ctx, name, passphrase, meta.RoleAdmin, mk)
	if err != nil {
		return Identity{}, err
	}
	if err := s.adoptMasterKey(mk); err != nil {
		return Identity{}, err
	}
	ident := Identity{UserID: id, Name: name, Role: meta.RoleAdmin}
	return ident, s.Audit(ctx, id, ip, "bootstrap", "root admin "+name)
}

// CreateUser adds an account (admin only; requires the unlocked master key
// to cut the new user's keyslot).
func (s *Service) CreateUser(ctx context.Context, actor Identity, name, passphrase, role, ip string) (int64, error) {
	if actor.Role != meta.RoleAdmin {
		return 0, ErrDenied
	}
	mk, err := s.masterKey()
	if err != nil {
		return 0, err
	}
	id, err := s.insertUser(ctx, name, passphrase, role, mk)
	if err != nil {
		return 0, err
	}
	return id, s.Audit(ctx, actor.UserID, ip, "user_created", fmt.Sprintf("%s (role %s)", name, role))
}

// insertUser derives the keyslot + pass_hash and inserts the row.
func (s *Service) insertUser(ctx context.Context, name, passphrase, role string, mk []byte) (int64, error) {
	if name == "" {
		return 0, errors.New("auth: user name must not be empty")
	}
	if role != meta.RoleAdmin && role != meta.RoleEditor && role != meta.RoleReader {
		return 0, fmt.Errorf("auth: invalid role %q", role)
	}
	if len(passphrase) < 8 {
		return 0, ErrWeakPassphrase
	}
	params, err := newParams()
	if err != nil {
		return 0, err
	}
	kek, authHalf := deriveKeys(passphrase, params)
	wrapped, err := seal(kek, mk, aadMasterKey)
	if err != nil {
		return 0, err
	}
	now := s.now().Unix()
	return s.store.Users.OnCtx(ctx).
		Set(meta.UserName, name).Set(meta.UserRole, role).
		Set(meta.UserPassHash, []byte(encodeHash(params, authHalf))).
		Set(meta.UserMKWrapped, wrapped).
		Set(meta.UserDisabled, int64(0)).
		Set(meta.UserCreatedAt, now).Set(meta.UserUpdatedAt, now).
		Insert()
}

// SetUserRole changes an account's global role (admin only; last-admin
// guarded).
func (s *Service) SetUserRole(ctx context.Context, actor Identity, userID int64, role, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if role != meta.RoleAdmin && role != meta.RoleEditor && role != meta.RoleReader {
		return fmt.Errorf("auth: invalid role %q", role)
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	if target.Role == meta.RoleAdmin && role != meta.RoleAdmin {
		if err := s.guardLastAdmin(ctx, userID); err != nil {
			return err
		}
	}
	if err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, role).Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "user_role_changed", fmt.Sprintf("%s -> %s", target.Name, role))
}

// SetUserDisabled disables (or re-enables) an account and, when disabling,
// revokes its sessions. Disabling is the practical removal path — hard
// removal fails on FK references from connections/history (see RemoveUser).
func (s *Service) SetUserDisabled(ctx context.Context, actor Identity, userID int64, disabled bool, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	flag := int64(0)
	action := "user_enabled"
	if disabled {
		flag = 1
		action = "user_disabled"
		if target.Role == meta.RoleAdmin {
			if err := s.guardLastAdmin(ctx, userID); err != nil {
				return err
			}
		}
	}
	if err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserDisabled, flag).Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
		return err
	}
	if disabled {
		if err := s.revokeAllSessions(ctx, userID); err != nil {
			return err
		}
	}
	return s.Audit(ctx, actor.UserID, ip, action, target.Name)
}

// RemoveUser hard-deletes an account (admin only; last-admin guarded).
// Sessions and grants cascade; rows the user still owns (connections,
// history) make the delete fail with a foreign-key error — disable instead.
func (s *Service) RemoveUser(ctx context.Context, actor Identity, userID int64, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	if target.Role == meta.RoleAdmin {
		if err := s.guardLastAdmin(ctx, userID); err != nil {
			return err
		}
	}
	if err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Delete(); err != nil {
		if errors.Is(err, dao.ErrForeignKey) {
			return fmt.Errorf("auth: user %s still owns rows (connections/history) — disable the account instead: %w", target.Name, err)
		}
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "user_removed", target.Name)
}

// guardLastAdmin fails with ErrLastAdmin when userID is the only enabled
// admin account.
func (s *Service) guardLastAdmin(ctx context.Context, userID int64) error {
	admins, err := s.store.Users.OnCtx(ctx).
		With(meta.UserRole, meta.RoleAdmin).With(meta.UserDisabled, int64(0)).
		Select()
	if err != nil {
		return err
	}
	for _, a := range admins {
		if a.ID != userID {
			return nil // another enabled admin exists
		}
	}
	return ErrLastAdmin
}

// ChangePassphrase rotates the caller's own passphrase: the old one is
// verified and used to unwrap the master key, which is rewrapped under the
// new KEK.
func (s *Service) ChangePassphrase(ctx context.Context, actor Identity, oldPass, newPass, ip string) error {
	if len(newPass) < 8 {
		return ErrWeakPassphrase
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, actor.UserID).Get()
	if err != nil {
		return err
	}
	params, verifier, err := decodeHash(string(u.PassHash))
	if err != nil {
		return err
	}
	kek, authHalf := deriveKeys(oldPass, params)
	if !verifyAuthHalf(authHalf, verifier) {
		return ErrBadCredentials
	}
	mk, err := open(kek, u.MKWrapped, aadMasterKey)
	if err != nil {
		return ErrBadCredentials
	}
	if err := s.adoptMasterKey(mk); err != nil {
		return err
	}
	if err := s.rewrap(ctx, actor.UserID, newPass, mk); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "passphrase_changed", u.Name)
}

// ResetPassphrase sets a new passphrase for another user (admin only;
// requires the unlocked master key to cut a fresh keyslot). The recovery
// path for forgotten passphrases (ADR-0054 §1).
func (s *Service) ResetPassphrase(ctx context.Context, actor Identity, userID int64, newPass, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if len(newPass) < 8 {
		return ErrWeakPassphrase
	}
	mk, err := s.masterKey()
	if err != nil {
		return err
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	if err := s.rewrap(ctx, userID, newPass, mk); err != nil {
		return err
	}
	if err := s.revokeAllSessions(ctx, userID); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "passphrase_reset", target.Name)
}

// rewrap derives fresh params for newPass and updates pass_hash + keyslot.
func (s *Service) rewrap(ctx context.Context, userID int64, newPass string, mk []byte) error {
	params, err := newParams()
	if err != nil {
		return err
	}
	kek, authHalf := deriveKeys(newPass, params)
	wrapped, err := seal(kek, mk, aadMasterKey)
	if err != nil {
		return err
	}
	return s.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserPassHash, []byte(encodeHash(params, authHalf))).
		Set(meta.UserMKWrapped, wrapped).
		Set(meta.UserUpdatedAt, s.now().Unix()).
		Update()
}
