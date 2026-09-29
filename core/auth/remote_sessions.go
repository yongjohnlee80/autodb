package auth

import (
	"context"
	"errors"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// RevokeRemoteSessions revokes every live session bound to a remote device,
// and reports how many: the startup reconciliation.
//
// The daemon calls it at every start, before anything serves. No remote
// transport outlives the process that accepted it, so no device-bound token
// can still be in use: without this, a token whose reconnect-grace write was
// lost in a crash would stay live until its expiry, owned by a connection
// that no longer exists. A local session (device_id 0) is not touched.
func (s *Service) RevokeRemoteSessions(ctx context.Context) (int64, error) {
	var n uint64
	err := s.inTx(ctx, func(tx *dao.Transaction) error {
		live := func() dao.DAO[*meta.Session, meta.SessionField, int64] {
			return s.store.Sessions.On(tx).
				WithPredicate(dao.Gt(string(meta.SessDeviceID), int64(0))).
				With(meta.SessRevoked, int64(0))
		}
		var err error
		if n, err = live().Count(); err != nil || n == 0 {
			return err
		}
		return live().Set(meta.SessRevoked, int64(1)).Update()
	})
	return int64(n), err
}

// RemoteKeyOwner answers the remote listener's question about an offered SSH
// key: whose live registered key has this fingerprint. The key must not be
// revoked and its user must exist and not be disabled; anything else is
// remote.ErrUnknownKey, one answer for every refusal, so the listener cannot
// be used to learn which fingerprints are registered.
func (s *Service) RemoteKeyOwner(ctx context.Context, fingerprint string) (keyID, userID int64, err error) {
	key, err := s.store.SSHKeys.OnCtx(ctx).
		With(meta.SSHKeyFingerprint, fingerprint).With(meta.SSHKeyRevokedAt, int64(0)).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return 0, 0, remote.ErrUnknownKey
	}
	if err != nil {
		return 0, 0, err
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, key.UserID).Get()
	if errors.Is(err, dao.ErrNoRows) || (err == nil && u.Disabled != 0) {
		return 0, 0, remote.ErrUnknownKey
	}
	if err != nil {
		return 0, 0, err
	}
	return key.ID, u.ID, nil
}
