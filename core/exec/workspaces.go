package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// Workspace management (ADR-0057 §5/§6): workspaces group connections.
// Writes are admin-gated with the mutation and its audit row in ONE meta
// transaction (R2), failures surfaced (R6). Reads are token-FILTERED
// projections (R13): a non-admin sees, inside any workspace, only the
// connections their effective grant covers at reader or above — and
// workspaces that are empty after that filter are omitted entirely
// (workspace names can reveal topology; absence is the secure default).

// ErrWorkspaceNotFound reports a missing workspace id.
var ErrWorkspaceNotFound = errors.New("exec: workspace not found")

// WorkspaceConnRef is one connection visible inside a workspace view.
type WorkspaceConnRef struct {
	ID     int64
	Name   string
	Engine engine.Name
}

// WorkspaceView is one workspace as seen by the calling token.
type WorkspaceView struct {
	ID          int64
	Name        string
	CreatedAt   int64
	Connections []WorkspaceConnRef
}

// CreateWorkspace creates a named workspace (admin).
func (e *Engine) CreateWorkspace(ctx context.Context, token, name, ip string) (int64, error) {
	ident, err := e.auth.Authorize(ctx, token, 0, auth.ActionManage)
	if err != nil {
		return 0, err
	}
	if name == "" {
		return 0, errors.New("exec: workspace name must not be empty")
	}
	var id int64
	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		var terr error
		id, terr = e.store.Workspaces.On(tx).
			Set(meta.WsName, name).
			Set(meta.WsCreatedAt, e.now().Unix()).
			Insert()
		if terr != nil {
			return terr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "workspace_create",
			fmt.Sprintf("workspace %d (%s)", id, name))
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// RenameWorkspace renames a workspace (admin).
func (e *Engine) RenameWorkspace(ctx context.Context, token string, wsID int64, name, ip string) error {
	ident, err := e.auth.Authorize(ctx, token, 0, auth.ActionManage)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("exec: workspace name must not be empty")
	}
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if _, terr := e.store.Workspaces.On(tx).With(meta.WsID, wsID).Get(); terr != nil {
			if errors.Is(terr, dao.ErrNoRows) {
				return ErrWorkspaceNotFound
			}
			return terr
		}
		if terr := e.store.Workspaces.On(tx).With(meta.WsID, wsID).
			Set(meta.WsName, name).Update(); terr != nil {
			return terr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "workspace_rename",
			fmt.Sprintf("workspace %d -> %s", wsID, name))
	})
}

// DeleteWorkspace removes a workspace (admin). Connection links cascade in
// the schema; SERVER-side deletion NEVER touches any frontend's local note
// files (ADR-0057 §5 — orphaned note directories surface as detached).
func (e *Engine) DeleteWorkspace(ctx context.Context, token string, wsID int64, ip string) error {
	ident, err := e.auth.Authorize(ctx, token, 0, auth.ActionManage)
	if err != nil {
		return err
	}
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if _, terr := e.store.Workspaces.On(tx).With(meta.WsID, wsID).Get(); terr != nil {
			if errors.Is(terr, dao.ErrNoRows) {
				return ErrWorkspaceNotFound
			}
			return terr
		}
		if terr := e.store.WorkspaceConns.On(tx).With(meta.WcWsID, wsID).Delete(); terr != nil {
			return terr
		}
		if terr := e.store.Workspaces.On(tx).With(meta.WsID, wsID).Delete(); terr != nil {
			return terr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "workspace_delete",
			fmt.Sprintf("workspace %d", wsID))
	})
}

// AttachConnection links a connection into a workspace (admin; idempotent —
// an existing link is left in place).
func (e *Engine) AttachConnection(ctx context.Context, token string, wsID, connID int64, ip string) error {
	ident, err := e.auth.Authorize(ctx, token, 0, auth.ActionManage)
	if err != nil {
		return err
	}
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if _, terr := e.store.Workspaces.On(tx).With(meta.WsID, wsID).Get(); terr != nil {
			if errors.Is(terr, dao.ErrNoRows) {
				return ErrWorkspaceNotFound
			}
			return terr
		}
		if _, terr := e.store.Connections.On(tx).With(meta.ConnID, connID).Get(); terr != nil {
			return terr
		}
		existing, terr := e.store.WorkspaceConns.On(tx).
			With(meta.WcWsID, wsID).With(meta.WcConnID, connID).Select()
		if terr != nil {
			return terr
		}
		if len(existing) == 0 {
			if _, terr := e.store.WorkspaceConns.On(tx).
				Set(meta.WcWsID, wsID).Set(meta.WcConnID, connID).Insert(); terr != nil {
				return terr
			}
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "workspace_attach",
			fmt.Sprintf("workspace %d += connection %d", wsID, connID))
	})
}

// DetachConnection unlinks a connection from a workspace (admin).
func (e *Engine) DetachConnection(ctx context.Context, token string, wsID, connID int64, ip string) error {
	ident, err := e.auth.Authorize(ctx, token, 0, auth.ActionManage)
	if err != nil {
		return err
	}
	return dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if terr := e.store.WorkspaceConns.On(tx).
			With(meta.WcWsID, wsID).With(meta.WcConnID, connID).Delete(); terr != nil {
			return terr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "workspace_detach",
			fmt.Sprintf("workspace %d -= connection %d", wsID, connID))
	})
}

// ListWorkspaces returns the workspaces visible to the token (R13):
// admins see every workspace and connection; everyone else sees only the
// connections their effective grant covers at reader or above, with
// empty-after-filter workspaces omitted. Authority is re-checked through
// the ONE gate (auth.Authorize) per candidate connection — no second
// permission model exists here.
func (e *Engine) ListWorkspaces(ctx context.Context, token string) ([]WorkspaceView, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	isAdmin := ident.Role() == meta.RoleAdmin

	wss, err := e.store.Workspaces.OnCtx(ctx).Select()
	if err != nil {
		return nil, err
	}
	links, err := e.store.WorkspaceConns.OnCtx(ctx).Select()
	if err != nil {
		return nil, err
	}
	conns, err := e.store.Connections.OnCtx(ctx).Select()
	if err != nil {
		return nil, err
	}
	connByID := make(map[int64]*meta.Connection, len(conns))
	for _, c := range conns {
		connByID[c.ID] = c
	}

	// Resolve visibility per connection once (the grant model is the only
	// authority; ErrDenied filters, other failures propagate).
	visible := make(map[int64]bool, len(connByID))
	for id := range connByID {
		if isAdmin {
			visible[id] = true
			continue
		}
		_, aerr := e.auth.Authorize(ctx, token, id, auth.ActionRead)
		switch {
		case aerr == nil:
			visible[id] = true
		case errors.Is(aerr, auth.ErrDenied):
			visible[id] = false
		default:
			return nil, aerr
		}
	}

	byWs := make(map[int64][]WorkspaceConnRef)
	for _, l := range links {
		c, ok := connByID[l.ConnectionID]
		if !ok || !visible[l.ConnectionID] {
			continue
		}
		byWs[l.WorkspaceID] = append(byWs[l.WorkspaceID],
			WorkspaceConnRef{ID: c.ID, Name: c.Name, Engine: c.Engine})
	}

	out := make([]WorkspaceView, 0, len(wss))
	for _, w := range wss {
		refs := byWs[w.ID]
		if !isAdmin && len(refs) == 0 {
			continue // omit empty-after-filter workspaces (R13)
		}
		out = append(out, WorkspaceView{
			ID: w.ID, Name: w.Name, CreatedAt: w.CreatedAt, Connections: refs,
		})
	}
	return out, nil
}
