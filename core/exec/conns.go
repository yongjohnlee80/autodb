package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/mysql"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// target returns the cached dao connection for connID, opening it on first
// use: decrypt the identity-bound DSN (ErrLocked before any passphrase
// login), open the engine's driver, and probe with SELECT 1.
func (e *Engine) target(ctx context.Context, connID int64, row *meta.Connection) (dao.DataConn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.conns[connID]; ok {
		return c, nil
	}

	dsn, err := e.auth.DecryptSecret(row.DSNEnc, connID)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("target-%d", connID)
	var conn dao.DataConn
	switch row.Engine {
	case "postgres":
		conn, err = postgres.OpenNamed(ctx, name, string(dsn))
	case "mysql":
		conn, err = mysql.OpenNamed(ctx, name, string(dsn))
	case "sqlite":
		conn, err = sqlite.OpenNamed(ctx, name, string(dsn))
	default:
		return nil, fmt.Errorf("exec: connection %d has unknown engine %q", connID, row.Engine)
	}
	if err != nil {
		return nil, fmt.Errorf("exec: opening connection %q: %w", row.Name, err)
	}
	// postgres.Open builds the pool without pinging; probe uniformly.
	rows, perr := conn.QueryContext(ctx, "SELECT 1")
	if perr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("exec: probing connection %q: %w", row.Name, perr)
	}
	_ = rows.Close()
	e.conns[connID] = conn
	return conn, nil
}

// closeTarget drops a cached connection (used on delete).
func (e *Engine) closeTarget(connID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.conns[connID]; ok {
		_ = c.Close()
		delete(e.conns, connID)
	}
}

// CreateConnection stores a managed connection with its DSN encrypted at
// rest, bound to the new row's id (ADR-0054 rev 1 must-fix #5): the row is
// inserted with an empty secret, the id seals the AAD, and the ciphertext
// lands in the same transaction as the creator's auto-grant and the audit
// rows. Global editors and admins may create (Objective 14).
func (e *Engine) CreateConnection(ctx context.Context, token, name, engineName, dsn, ip string) (int64, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return 0, err
	}
	if ident.Role() != meta.RoleAdmin && ident.Role() != meta.RoleEditor {
		return 0, auth.ErrDenied
	}
	switch engineName {
	case "postgres", "mysql", "sqlite":
	default:
		return 0, fmt.Errorf("exec: unsupported connection engine %q (postgres, mysql, sqlite)", engineName)
	}
	if name == "" || dsn == "" {
		return 0, errors.New("exec: connection name and dsn must not be empty")
	}
	if !e.auth.Unlocked() {
		return 0, auth.ErrLocked
	}
	var id int64
	err = dao.RunTx(ctx, []dao.DataConn{e.store.Conn()}, func(tx *dao.Transaction) error {
		now := e.now().Unix()
		var terr error
		id, terr = e.store.Connections.On(tx).
			Set(meta.ConnName, name).Set(meta.ConnEngine, engineName).
			Set(meta.ConnDSNEnc, []byte{}).Set(meta.ConnCreatedBy, ident.UserID()).
			Set(meta.ConnCreatedAt, now).Set(meta.ConnUpdatedAt, now).
			Insert()
		if terr != nil {
			return terr
		}
		enc, terr := e.auth.EncryptSecret([]byte(dsn), id)
		if terr != nil {
			return terr
		}
		if terr := e.store.Connections.On(tx).With(meta.ConnID, id).
			Set(meta.ConnDSNEnc, enc).Update(); terr != nil {
			return terr
		}
		if terr := e.auth.GrantCreatorTx(tx, ident, id); terr != nil {
			return fmt.Errorf("exec: auto-granting creator: %w", terr)
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "connection_created",
			fmt.Sprintf("%s (%s), creator auto-granted %s", name, engineName, ident.Role()))
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListConnections returns the connections visible to the token's user —
// all of them for admins, granted ones otherwise. DSN ciphertext is zeroed
// in every return: plaintext DSNs never leave the engine.
func (e *Engine) ListConnections(ctx context.Context, token string) ([]*meta.Connection, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	var rows []*meta.Connection
	if ident.Role() == meta.RoleAdmin {
		rows, err = e.store.Connections.OnCtx(ctx).Select()
	} else {
		grants, gerr := e.store.Grants.OnCtx(ctx).With(meta.GrantUserID, ident.UserID()).Select()
		if gerr != nil {
			return nil, gerr
		}
		if len(grants) == 0 {
			return nil, nil
		}
		ids := make([]any, len(grants))
		for i, g := range grants {
			ids[i] = g.ConnectionID
		}
		rows, err = e.store.Connections.OnCtx(ctx).With(meta.ConnID, ids...).Select()
	}
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		r.DSNEnc = nil
	}
	return rows, nil
}

// DeleteConnection removes a managed connection (admin token). Grants and
// workspace links cascade; history rows refuse the delete (FK) — the
// record outlives the connection by design.
func (e *Engine) DeleteConnection(ctx context.Context, token string, connID int64, ip string) error {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return err
	}
	if ident.Role() != meta.RoleAdmin {
		return auth.ErrDenied
	}
	row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if err != nil {
		return err
	}
	e.closeTarget(connID)
	return dao.RunTx(ctx, []dao.DataConn{e.store.Conn()}, func(tx *dao.Transaction) error {
		if err := e.store.Connections.On(tx).With(meta.ConnID, connID).Delete(); err != nil {
			if errors.Is(err, dao.ErrForeignKey) {
				return fmt.Errorf("exec: connection %q has recorded history and cannot be deleted: %w", row.Name, err)
			}
			return err
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "connection_deleted", row.Name)
	})
}

// TestConnection authorizes read access and probes the target with SELECT 1.
func (e *Engine) TestConnection(ctx context.Context, token string, connID int64) error {
	row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return auth.ErrDenied
	}
	if err != nil {
		return err
	}
	if _, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead); err != nil {
		return err
	}
	conn, err := e.target(ctx, connID, row)
	if err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "SELECT 1")
	if err != nil {
		return fmt.Errorf("exec: probe failed: %w", err)
	}
	return rows.Close()
}
