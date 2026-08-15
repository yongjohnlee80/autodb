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

// Authorize is the single permission gate (ADR-0054 §3). ActionManage
// checks the global role alone. Connection-scoped actions require a grant —
// for admins too (Objective 13) — and the effective role is
// min(global role, grant role): a globally-reader user never exceeds
// SELECT regardless of grants (Objective 15).
func (s *Service) Authorize(ctx context.Context, ident Identity, connID int64, action Action) error {
	if action == ActionManage {
		if ident.Role != meta.RoleAdmin {
			return ErrDenied
		}
		return nil
	}
	g, err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, ident.UserID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	eff := min(rankOf(ident.Role), rankOf(g.Role))
	if eff < requiredRank(action) {
		return ErrDenied
	}
	return nil
}

// AddGrant gives userID a role on connID (admin only; Objective 13 —
// including granting to oneself). An existing grant is updated in place.
func (s *Service) AddGrant(ctx context.Context, actor Identity, userID, connID int64, role, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if rankOf(role) == 0 {
		return fmt.Errorf("auth: invalid grant role %q", role)
	}
	_, err := s.store.Grants.OnCtx(ctx).
		Set(meta.GrantUserID, userID).Set(meta.GrantConnID, connID).
		Set(meta.GrantRole, role).Set(meta.GrantGrantedBy, actor.UserID).
		Set(meta.GrantCreatedAt, s.now().Unix()).
		Insert()
	if errors.Is(err, dao.ErrDuplicate) {
		err = s.store.Grants.OnCtx(ctx).
			With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).
			Set(meta.GrantRole, role).Set(meta.GrantGrantedBy, actor.UserID).
			Update()
	}
	if err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "grant_added",
		fmt.Sprintf("user %d on connection %d as %s", userID, connID, role))
}

// RemoveGrant revokes userID's grant on connID (admin only).
func (s *Service) RemoveGrant(ctx context.Context, actor Identity, userID, connID int64, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Delete(); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "grant_removed",
		fmt.Sprintf("user %d on connection %d", userID, connID))
}
