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

// resolveToken is the single provenance check: token → live session → live
// user → fresh Identity. Every privileged method calls it, so authority is
// re-read from the store on every call — a demotion or disable takes effect
// on the caller's next request (lector M3 must-fix #1).
func (s *Service) resolveToken(ctx context.Context, token string) (Identity, *meta.Session, error) {
	sess, err := s.store.Sessions.OnCtx(ctx).With(meta.SessTokenHash, tokenHash(token)).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return Identity{}, nil, ErrTokenInvalid
	}
	if err != nil {
		return Identity{}, nil, err
	}
	if sess.Revoked != 0 || s.now().Unix() >= sess.ExpiresAt {
		return Identity{}, nil, ErrTokenInvalid
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, sess.UserID).Get()
	if err != nil {
		return Identity{}, nil, err
	}
	if u.Disabled != 0 {
		return Identity{}, nil, ErrTokenInvalid
	}
	return Identity{userID: u.ID, name: u.Name, role: u.Role}, sess, nil
}

// ValidateToken resolves a session token to a fresh Identity.
func (s *Service) ValidateToken(ctx context.Context, token string) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, token)
	return ident, err
}

// requireAdmin resolves the token and demands a current admin role.
func (s *Service) requireAdmin(ctx context.Context, token string) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if ident.role != meta.RoleAdmin {
		return Identity{}, ErrDenied
	}
	return ident, nil
}

// newSessionTx inserts a session row inside tx and returns the one-time
// token string.
func (s *Service) newSessionTx(tx *dao.Transaction, userID int64, ip string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if _, err := s.store.Sessions.On(tx).
		Set(meta.SessTokenHash, tokenHash(token)).
		Set(meta.SessUserID, userID).Set(meta.SessIP, ip).
		Set(meta.SessCreatedAt, now.Unix()).
		Set(meta.SessExpiresAt, now.Add(s.ttl).Unix()).
		Set(meta.SessRevoked, int64(0)).
		Insert(); err != nil {
		return "", err
	}
	return token, nil
}

// Login authenticates name+passphrase from ip and issues a session token.
// The session insert and the audit row commit atomically; the master key is
// installed in memory only after the commit (must-fix #2). Failures are
// audited and deliberately indistinguishable (ErrBadCredentials), except a
// disallowed IP (ErrDenied).
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
	if len(u.MKWrapped) == 0 {
		// A v1-era row that never received a keyslot: fail explicitly — an
		// admin passphrase reset cuts one (lector M3 should-fix).
		return "", Identity{}, ErrNoKeyslot
	}
	mk, err := open(kek, u.MKWrapped, aadMasterKey)
	if err != nil {
		return "", Identity{}, fmt.Errorf("auth: unwrapping keyslot for %s: %w", name, ErrKeyslotCorrupt)
	}
	if err := s.checkKeyConsistency(mk); err != nil {
		return "", Identity{}, err
	}

	var token string
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		var terr error
		token, terr = s.newSessionTx(tx, u.ID, ip)
		if terr != nil {
			return terr
		}
		return s.AuditTx(tx, u.ID, ip, "login", name)
	})
	if err != nil {
		return "", Identity{}, err
	}
	s.adoptMasterKey(mk) // memory effect only after commit

	return token, Identity{userID: u.ID, name: u.Name, role: u.Role}, nil
}

// Logout revokes the calling session (kept as a row for audit).
func (s *Service) Logout(ctx context.Context, token, ip string) error {
	ident, sess, err := s.resolveToken(ctx, token)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.store.Sessions.On(tx).With(meta.SessID, sess.ID).
			Set(meta.SessRevoked, int64(1)).Update(); err != nil {
			return err
		}
		return s.AuditTx(tx, ident.userID, ip, "logout", "")
	})
}

// RevokeUserSessions revokes every session of userID (self, or admin).
func (s *Service) RevokeUserSessions(ctx context.Context, token string, userID int64, ip string) error {
	actor, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return err
	}
	if actor.userID != userID && actor.role != meta.RoleAdmin {
		return ErrDenied
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.revokeAllSessionsTx(tx, userID, 0); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "sessions_revoked", fmt.Sprintf("user %d", userID))
	})
}

// revokeAllSessionsTx revokes userID's sessions inside tx, sparing
// exceptSessID when non-zero (the caller's own session on passphrase
// change).
func (s *Service) revokeAllSessionsTx(tx *dao.Transaction, userID, exceptSessID int64) error {
	q := s.store.Sessions.On(tx).With(meta.SessUserID, userID)
	if exceptSessID != 0 {
		q = q.Excluding(meta.SessID, exceptSessID)
	}
	return q.Set(meta.SessRevoked, int64(1)).Update()
}
