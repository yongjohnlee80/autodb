package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Action classifies what a caller wants to do (consumed by the M4 execution
// engine's statement classifier).
type Action string

const (
	// ActionRead is SELECT-class access (reader and above).
	ActionRead Action = "read"
	// ActionWrite is DML — INSERT/UPDATE/DELETE (editor and above).
	ActionWrite Action = "write"
	// ActionDDL is ALTER/CREATE/DROP (editor and above, Objective 14).
	ActionDDL Action = "ddl"
	// ActionManage is administration — users, grants, allowlist (admin
	// only; not connection-scoped).
	ActionManage Action = "manage"
)

// rankOf orders roles: reader < editor < admin. Unknown roles rank 0.
func rankOf(role string) int {
	switch role {
	case meta.RoleReader:
		return 1
	case meta.RoleEditor:
		return 2
	case meta.RoleAdmin:
		return 3
	}
	return 0
}

// requiredRank maps an action to its minimum effective role rank.
func requiredRank(a Action) int {
	switch a {
	case ActionRead:
		return 1
	case ActionWrite, ActionDDL:
		return 2
	case ActionManage:
		return 3
	}
	return int(^uint(0) >> 1) // unknown action: unreachable rank
}

// Authorize is the single permission gate (ADR-0054 §3): it resolves the
// token to a FRESH identity (provenance + current authority, must-fix #1)
// and checks the action. ActionManage checks the global role alone.
// Connection-scoped actions require a grant — for admins too (Objective
// 13) — and the effective role is min(global role, grant role): a
// globally-reader user never exceeds SELECT regardless of grants
// (Objective 15). The fresh identity is returned for the caller's records.
func (s *Service) Authorize(ctx context.Context, token string, connID int64, action Action) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if action == ActionManage {
		if ident.role != meta.RoleAdmin {
			return Identity{}, ErrDenied
		}
		return ident, nil
	}
	g, err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, ident.userID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return Identity{}, ErrDenied
	}
	if err != nil {
		return Identity{}, err
	}
	eff := min(rankOf(ident.role), rankOf(g.Role))
	if eff < requiredRank(action) {
		return Identity{}, ErrDenied
	}
	return ident, nil
}

// AddGrant gives userID a role on connID (admin token; Objective 13 —
// including granting to oneself). An existing grant is updated in place.
func (s *Service) AddGrant(ctx context.Context, token string, userID, connID int64, role, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if rankOf(role) == 0 {
		return fmt.Errorf("auth: invalid grant role %q", role)
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		_, err := s.store.Grants.On(tx).
			Set(meta.GrantUserID, userID).Set(meta.GrantConnID, connID).
			Set(meta.GrantRole, role).Set(meta.GrantGrantedBy, actor.userID).
			Set(meta.GrantCreatedAt, s.now().Unix()).
			Insert()
		if errors.Is(err, dao.ErrDuplicate) {
			err = s.store.Grants.On(tx).
				With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).
				Set(meta.GrantRole, role).Set(meta.GrantGrantedBy, actor.userID).
				Update()
		}
		if err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "grant_added",
			fmt.Sprintf("user %d on connection %d as %s", userID, connID, role))
	})
}

// RemoveGrant revokes userID's grant on connID (admin token).
func (s *Service) RemoveGrant(ctx context.Context, token string, userID, connID int64, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.store.Grants.On(tx).
			With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Delete(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "grant_removed",
			fmt.Sprintf("user %d on connection %d", userID, connID))
	})
}

// GrantCreatorTx writes the auto-grant a connection creator receives
// (ADR-0055 §3 — flagged decision), inside the creation transaction.
func (s *Service) GrantCreatorTx(tx *dao.Transaction, creator Identity, connID int64) error {
	_, err := s.store.Grants.On(tx).
		Set(meta.GrantUserID, creator.userID).Set(meta.GrantConnID, connID).
		Set(meta.GrantRole, creator.role).Set(meta.GrantGrantedBy, creator.userID).
		Set(meta.GrantCreatedAt, s.now().Unix()).Insert()
	return err
}
