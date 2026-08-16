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

// Bootstrap creates the install master key and the root admin, logs the
// root in (returning the session token), and unlocks the service. The
// users==0 invariant is re-checked inside the guarded transaction, so
// concurrent bootstraps cannot both succeed (must-fix #3).
func (s *Service) Bootstrap(ctx context.Context, name, passphrase, ip string) (string, Identity, error) {
	mk, err := newKey()
	if err != nil {
		return "", Identity{}, err
	}
	var (
		token string
		id    int64
	)
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.lockGuardRow(tx); err != nil {
			return err
		}
		n, err := s.store.Users.On(tx).Count()
		if err != nil {
			return err
		}
		if n != 0 {
			return ErrBootstrapDone
		}
		id, err = s.insertUserTx(tx, name, passphrase, meta.RoleAdmin, mk)
		if err != nil {
			return err
		}
		token, err = s.newSessionTx(tx, id, ip)
		if err != nil {
			return err
		}
		if err := s.AuditTx(tx, id, ip, "bootstrap", "root admin "+name); err != nil {
			return err
		}
		return s.AuditTx(tx, id, ip, "login", name)
	})
	if err != nil {
		return "", Identity{}, err
	}
	s.adoptMasterKey(mk)
	return token, Identity{userID: id, name: name, role: meta.RoleAdmin}, nil
}

// CreateUser adds an account (admin token; requires the unlocked master key
// to cut the new user's keyslot).
func (s *Service) CreateUser(ctx context.Context, token, name, passphrase, role, ip string) (int64, error) {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return 0, err
	}
	mk, err := s.masterKey()
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		var terr error
		id, terr = s.insertUserTx(tx, name, passphrase, role, mk)
		if terr != nil {
			return terr
		}
		return s.AuditTx(tx, actor.userID, ip, "user_created", fmt.Sprintf("%s (role %s)", name, role))
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// insertUserTx derives the keyslot + pass_hash and inserts the row in tx.
func (s *Service) insertUserTx(tx *dao.Transaction, name, passphrase, role string, mk []byte) (int64, error) {
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
	return s.store.Users.On(tx).
		Set(meta.UserName, name).Set(meta.UserRole, role).
		Set(meta.UserPassHash, []byte(encodeHash(params, authHalf))).
		Set(meta.UserMKWrapped, wrapped).
		Set(meta.UserDisabled, int64(0)).
		Set(meta.UserCreatedAt, now).Set(meta.UserUpdatedAt, now).
		Insert()
}

// adminsRemainTx reports whether at least one enabled admin exists in the
// transaction's view — the post-mutation invariant recheck (must-fix #3).
func (s *Service) adminsRemainTx(tx *dao.Transaction) (bool, error) {
	n, err := s.store.Users.On(tx).
		With(meta.UserRole, meta.RoleAdmin).With(meta.UserDisabled, int64(0)).
		Count()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetUserRole changes an account's global role (admin token). Demotions
// re-verify the enabled-admin invariant inside the guarded transaction.
func (s *Service) SetUserRole(ctx context.Context, token string, userID int64, role, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if role != meta.RoleAdmin && role != meta.RoleEditor && role != meta.RoleReader {
		return fmt.Errorf("auth: invalid role %q", role)
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.lockGuardRow(tx); err != nil {
			return err
		}
		if err := s.store.Users.On(tx).With(meta.UserID, userID).
			Set(meta.UserRole, role).Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
			return err
		}
		ok, err := s.adminsRemainTx(tx)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLastAdmin
		}
		return s.AuditTx(tx, actor.userID, ip, "user_role_changed", fmt.Sprintf("%s -> %s", target.Name, role))
	})
}

// SetUserDisabled disables (or re-enables) an account; disabling revokes
// its sessions and re-verifies the enabled-admin invariant in-tx.
func (s *Service) SetUserDisabled(ctx context.Context, token string, userID int64, disabled bool, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	flag, action := int64(0), "user_enabled"
	if disabled {
		flag, action = 1, "user_disabled"
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.lockGuardRow(tx); err != nil {
			return err
		}
		if err := s.store.Users.On(tx).With(meta.UserID, userID).
			Set(meta.UserDisabled, flag).Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
			return err
		}
		if disabled {
			ok, err := s.adminsRemainTx(tx)
			if err != nil {
				return err
			}
			if !ok {
				return ErrLastAdmin
			}
			if err := s.revokeAllSessionsTx(tx, userID, 0); err != nil {
				return err
			}
		}
		return s.AuditTx(tx, actor.userID, ip, action, target.Name)
	})
}

// RemoveUser hard-deletes an account (admin token). Sessions and grants
// cascade; rows the user still owns (connections, history) make the delete
// fail with a foreign-key error — disable instead. The enabled-admin
// invariant is re-verified in-tx.
func (s *Service) RemoveUser(ctx context.Context, token string, userID int64, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	target, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.lockGuardRow(tx); err != nil {
			return err
		}
		if err := s.store.Users.On(tx).With(meta.UserID, userID).Delete(); err != nil {
			if errors.Is(err, dao.ErrForeignKey) {
				return fmt.Errorf("auth: user %s still owns rows (connections/history) — disable the account instead: %w", target.Name, err)
			}
			return err
		}
		ok, err := s.adminsRemainTx(tx)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLastAdmin
		}
		return s.AuditTx(tx, actor.userID, ip, "user_removed", target.Name)
	})
}

// ChangePassphrase rotates the calling token's own passphrase: the old one
// is verified and used to unwrap the master key, which is rewrapped under
// the new KEK. Every OTHER session of the user is revoked (lector M3
// should-fix); the calling session stays live.
func (s *Service) ChangePassphrase(ctx context.Context, token, oldPass, newPass, ip string) error {
	actor, sess, err := s.resolveToken(ctx, token)
	if err != nil {
		return err
	}
	if len(newPass) < 8 {
		return ErrWeakPassphrase
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, actor.userID).Get()
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
	if len(u.MKWrapped) == 0 {
		return ErrNoKeyslot
	}
	mk, err := open(kek, u.MKWrapped, aadMasterKey)
	if err != nil {
		return ErrBadCredentials
	}
	if err := s.checkKeyConsistency(mk); err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.rewrapTx(tx, actor.userID, newPass, mk); err != nil {
			return err
		}
		if err := s.revokeAllSessionsTx(tx, actor.userID, sess.ID); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "passphrase_changed", u.Name)
	})
	if err != nil {
		return err
	}
	s.adoptMasterKey(mk)
	return nil
}

// ResetPassphrase sets a new passphrase for another user (admin token;
// requires the unlocked master key to cut a fresh keyslot). The recovery
// path for forgotten passphrases (ADR-0054 §1); all target sessions revoke.
func (s *Service) ResetPassphrase(ctx context.Context, token string, userID int64, newPass, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
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
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.rewrapTx(tx, userID, newPass, mk); err != nil {
			return err
		}
		if err := s.revokeAllSessionsTx(tx, userID, 0); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "passphrase_reset", target.Name)
	})
}

// rewrapTx derives fresh params for newPass and updates pass_hash + keyslot
// inside tx.
func (s *Service) rewrapTx(tx *dao.Transaction, userID int64, newPass string, mk []byte) error {
	params, err := newParams()
	if err != nil {
		return err
	}
	kek, authHalf := deriveKeys(newPass, params)
	wrapped, err := seal(kek, mk, aadMasterKey)
	if err != nil {
		return err
	}
	return s.store.Users.On(tx).With(meta.UserID, userID).
		Set(meta.UserPassHash, []byte(encodeHash(params, authHalf))).
		Set(meta.UserMKWrapped, wrapped).
		Set(meta.UserUpdatedAt, s.now().Unix()).
		Update()
}
