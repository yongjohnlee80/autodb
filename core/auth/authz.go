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

// GrantCreatorTx writes the ownership grant a connection creator receives,
// inside the creation transaction. It is NOT general grant management:
//
//   - the actor is proven by token, re-resolved inside this call (no
//     caller-supplied identity — lector M3 r2 must-fix #1);
//   - the creator relationship is verified against the row just inserted
//     (connections.created_by must be the token's user);
//   - the granted role is capped at editor — min(global role, editor) — so
//     creation can never mint connection-admin rights; arbitrary grant
//     management stays admin-only via AddGrant (lector policy ruling).
func (s *Service) GrantCreatorTx(tx *dao.Transaction, token string, connID int64) (Identity, error) {
	// Resolve on the transaction: the caller already holds it, and pool
	// access here would deadlock single-connection stores.
	actor, err := s.resolveTokenTx(tx, token)
	if err != nil {
		return Identity{}, err
	}
	row, err := s.store.Connections.On(tx).With(meta.ConnID, connID).Get()
	if err != nil {
		return Identity{}, err
	}
	if row.CreatedBy != actor.userID {
		return Identity{}, fmt.Errorf("%w: creator grant is only available to the connection's creator", ErrDenied)
	}
	// Require current editor+ INSIDE the transaction: a demotion to reader
	// that commits between the outer CreateConnection check and here must
	// roll the whole creation back, not silently grant reader on a
	// connection a reader may not create (lector M4 r2 must-fix #4).
	if rankOf(actor.role) < rankOf(meta.RoleEditor) {
		return Identity{}, fmt.Errorf("%w: creating a connection requires editor or admin", ErrDenied)
	}
	// The ownership grant is capped at editor — creation never mints
	// connection-admin rights (lector policy ruling).
	role := meta.RoleEditor
	if _, err := s.store.Grants.On(tx).
		Set(meta.GrantUserID, actor.userID).Set(meta.GrantConnID, connID).
		Set(meta.GrantRole, role).Set(meta.GrantGrantedBy, actor.userID).
		Set(meta.GrantCreatedAt, s.now().Unix()).Insert(); err != nil {
		return Identity{}, err
	}
	return actor, nil
}

// StillAuthorized re-checks a standing authority WITHOUT a token.
//
// It exists for held resources — an ExecSession with a pinned transaction —
// where the authority was established once, at open, and the resource then
// outlives the call that created it. Authorize cannot serve that: it starts
// from a token the holder is not currently presenting, and the whole question
// is what happens when nobody is presenting anything.
//
// Every way an authority can end is checked here, because a rollback that
// fires for a removed grant but not for a disabled user is not a guarantee,
// it is a coincidence:
//
//   - the auth session was revoked (logout, or an admin revoking the user's
//     sessions),
//   - it expired,
//   - the user was disabled,
//   - the grant on this connection was removed or lowered below what the
//     action needs.
//
// A nil error means the authority still stands. ErrDenied means it does not,
// whatever ended it; a store failure is returned as itself, because "the
// database is unreachable" must not be reported as "you are not allowed".
func (s *Service) StillAuthorized(ctx context.Context, authSessID, userID, connID int64, action Action) error {
	sess, err := s.store.Sessions.OnCtx(ctx).With(meta.SessID, authSessID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if sess.Revoked != 0 || s.now().Unix() >= sess.ExpiresAt || sess.UserID != userID {
		return ErrDenied
	}
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if u.Disabled != 0 {
		return ErrDenied
	}
	if action == ActionManage {
		if u.Role != meta.RoleAdmin {
			return ErrDenied
		}
		return nil
	}
	g, err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if min(rankOf(u.Role), rankOf(g.Role)) < requiredRank(action) {
		return ErrDenied
	}
	return nil
}

// SessionRef resolves a token to its identity AND the auth-session row id, so
// a caller holding a long-lived resource can re-check that authority later
// with StillAuthorized. The id is not a credential — it names a row, and the
// row is what carries revocation and expiry.
func (s *Service) SessionRef(ctx context.Context, token string) (Identity, int64, error) {
	ident, sess, err := s.resolveToken(ctx, token)
	if err != nil {
		return Identity{}, 0, err
	}
	return ident, sess.ID, nil
}
