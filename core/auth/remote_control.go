package auth

import (
	"context"
	"errors"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// SetRemoteControl records the Remote Control switch and, when it changes,
// audits who changed it (remote_control_changed), in one transaction: a
// change is never recorded without its audit row, nor audited without being
// made. It reports whether the switch changed. Who may change it is the
// caller's question.
func (s *Service) SetRemoteControl(ctx context.Context, on bool, byUserID int64, ip string) (bool, error) {
	next := "off"
	if on {
		next = "on"
	}
	changed := false
	err := s.inTx(ctx, func(tx *dao.Transaction) error {
		cur, err := s.store.KV.On(tx).With(meta.KVKey, remote.ControlKey).Get()
		if err != nil && !errors.Is(err, dao.ErrNoRows) {
			return err
		}
		if (cur != nil && cur.Value == "on") == on {
			return nil
		}
		if err := s.store.KV.On(tx).Set(meta.KVKey, remote.ControlKey).Set(meta.KVValue, next).Upsert(); err != nil {
			return err
		}
		changed = true
		return s.AuditTx(tx, byUserID, ip, "remote_control_changed", "remote control "+next)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
