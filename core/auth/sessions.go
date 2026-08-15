package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// tokenHash maps the opaque token string to its stored digest — the store
// never holds a usable token (Objective 20).
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Login authenticates name+passphrase from ip, unlocks the master key, and
// issues a session token (returned once; only its hash is stored). Failures
// are audited and deliberately indistinguishable (ErrBadCredentials), except
// a disallowed IP (ErrDenied).
func (s *Service) Login(ctx context.Context, name, passphrase, ip string) (string, Identity, error) {
	allowed, err := s.IPAllowed(ctx, ip)
	if err != nil {
		return "", Identity{}, err
	}
	if !allowed {
		if err := s.Audit(ctx, 0, ip, "login_failed", name+" (ip not allowed)"); err != nil {
			return "", Identity{}, err
		}
		return "", Identity{}, ErrDenied
	}

	u, err := s.store.Users.OnCtx(ctx).With(meta.UserName, name).Get()
	if errors.Is(err, dao.ErrNoRows) {
		dummyDerive(passphrase) // equalize timing for unknown users
		if aerr := s.Audit(ctx, 0, ip, "login_failed", name+" (unknown user)"); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}
	if err != nil {
		return "", Identity{}, err
	}
	if u.Disabled != 0 {
		dummyDerive(passphrase)
		if aerr := s.Audit(ctx, u.ID, ip, "login_failed", name+" (disabled)"); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}

	params, verifier, err := decodeHash(string(u.PassHash))
	if err != nil {
		return "", Identity{}, err
	}
	kek, authHalf := deriveKeys(passphrase, params)
	if !verifyAuthHalf(authHalf, verifier) {
		if aerr := s.Audit(ctx, u.ID, ip, "login_failed", name); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}
	mk, err := open(kek, u.MKWrapped, aadMasterKey)
	if err != nil {
		return "", Identity{}, fmt.Errorf("auth: unwrapping keyslot for %s: %w", name, ErrKeyslotCorrupt)
	}
	if err := s.adoptMasterKey(mk); err != nil {
		return "", Identity{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Identity{}, fmt.Errorf("auth: generating token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if _, err := s.store.Sessions.OnCtx(ctx).
		Set(meta.SessTokenHash, tokenHash(token)).
		Set(meta.SessUserID, u.ID).Set(meta.SessIP, ip).
		Set(meta.SessCreatedAt, now.Unix()).
		Set(meta.SessExpiresAt, now.Add(s.ttl).Unix()).
		Set(meta.SessRevoked, int64(0)).
		Insert(); err != nil {
		return "", Identity{}, err
	}

	ident := Identity{UserID: u.ID, Name: u.Name, Role: u.Role}
	if err := s.Audit(ctx, u.ID, ip, "login", name); err != nil {
		return "", Identity{}, err
	}
	return token, ident, nil
}

// ValidateToken resolves a session token to an Identity: known hash, not
// expired, not revoked, account enabled. The client IP is recorded at login;
// tokens are not IP-pinned in v1 (ADR-0054 §2).
func (s *Service) ValidateToken(ctx context.Context, token string) (Identity, error) {
	sess, err := s.store.Sessions.OnCtx(ctx).With(meta.SessTokenHash, tokenHash(token)).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return Identity{}, ErrTokenInvalid
	}
	if err != nil {
		return Identity{}, err
	}
	if sess.Revoked != 0 || s.now().Unix() >= sess.ExpiresAt {
		return Identity{}, ErrTokenInvalid
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, sess.UserID).Get()
	if err != nil {
		return Identity{}, err
	}
	if u.Disabled != 0 {
		return Identity{}, ErrTokenInvalid
	}
	return Identity{UserID: u.ID, Name: u.Name, Role: u.Role}, nil
}

// Logout revokes one session token (kept as a row for audit).
func (s *Service) Logout(ctx context.Context, token, ip string) error {
	sess, err := s.store.Sessions.OnCtx(ctx).With(meta.SessTokenHash, tokenHash(token)).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrTokenInvalid
	}
	if err != nil {
		return err
	}
	if err := s.store.Sessions.OnCtx(ctx).With(meta.SessID, sess.ID).
		Set(meta.SessRevoked, int64(1)).Update(); err != nil {
		return err
	}
	return s.Audit(ctx, sess.UserID, ip, "logout", "")
}

// RevokeUserSessions revokes every session of userID (self, or admin).
func (s *Service) RevokeUserSessions(ctx context.Context, actor Identity, userID int64, ip string) error {
	if actor.UserID != userID && actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if err := s.revokeAllSessions(ctx, userID); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "sessions_revoked", fmt.Sprintf("user %d", userID))
}

func (s *Service) revokeAllSessions(ctx context.Context, userID int64) error {
	return s.store.Sessions.OnCtx(ctx).With(meta.SessUserID, userID).
		Set(meta.SessRevoked, int64(1)).Update()
}
