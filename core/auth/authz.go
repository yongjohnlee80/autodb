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
	if derr := s.decide(ctx, nil, ident.userID, ident.role, connID, action); derr != nil {
		return Identity{}, derr
	}
	return ident, nil
}

// decide is THE authorization rule, and the only copy of it.
//
// It takes an identity that has ALREADY been resolved and validated, because
// resolution is the part that legitimately differs: a session token resolves
// through a session row and its expiry, a PAT resolves through the token
// record and its owner, and neither can express the other's checks. What must
// not differ is what follows — whether manage requires admin, whether a grant
// is required at all, and how a user's role and a grant's role combine.
//
// The previous version had that rule written twice, once here and once in
// AuthorizeUser, under a comment claiming there was one place. A comment
// asserting a property the code does not have is worse than no comment,
// because the next person to change one copy reads it and believes the other
// followed. Lector caught it: two copies of a security rule are not a
// duplication smell, they are a future divergence with a date on it.
// decide resolves a grant into a yes/no for one action.
//
// It takes an OPTIONAL transaction and issues through On(tx) — nil is the pool
// by contract, the same shape canonicalAllowedIPs uses and for the same reason
// (security-core-hardening R14). A caller holding a transaction MUST pass it:
// reaching for the pool from inside one deadlocks a single-connection sqlite
// store and reads outside the transaction's snapshot on a file store. Minting
// a PAT gates on a grant while holding the cap locks, which is what made the
// parameter necessary.
func (s *Service) decide(ctx context.Context, tx *dao.Transaction, userID int64, role string, connID int64, action Action) error {
	if action == ActionManage {
		// Manage is an account-level power and is not delegated by a grant:
		// no per-connection grant can make a non-admin an administrator.
		if role != meta.RoleAdmin {
			return ErrDenied
		}
		return nil
	}
	g, err := s.store.Grants.On(tx, dao.WithQueryContext(ctx)).
		With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	// The LOWER of the two ranks. A grant cannot lift a user above their
	// account role, and an account role cannot reach past a narrower grant.
	if min(rankOf(role), rankOf(g.Role)) < requiredRank(action) {
		return ErrDenied
	}
	return nil
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

// StillAuthorized WAS HERE and is deliberately gone.
//
// It re-checked a standing authority without a token, which is the right
// idea, by opening with a lookup keyed on an AUTH SESSION ROW — which is only
// one of the two kinds of credential a session can stand on. A front-door
// session stands on a PAT and has no session row, so it passed a zero and the
// missing row read as a revocation.
//
// Deleting it rather than fixing it in place is the point. A function that
// takes a bare id and silently means "session table" is the shape of the
// defect; ResolveStanding takes a TYPED AuthorityRef, so the caller cannot
// fail to say which table the id came from and a future caller cannot
// reintroduce this by passing the wrong one. Leaving the old signature
// available would leave the trap available.
//
// See standing.go.

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
