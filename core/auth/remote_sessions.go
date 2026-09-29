package auth

import (
	"context"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
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
