package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// POOL ADMINISTRATION. Admin only. Each write validates, writes the meta store
// with its audit row in one transaction, then updates poolSet, then lets the
// session line serve whoever a raise can now admit. The store is written
// first, so a failed write changes nothing live.
//
// Each write is ONE lifecycle operation (lifecycleOp), from its first read to
// its last live effect. The store orders commits and poolSet orders
// publications, but nothing else orders the two: two updates could commit
// 95 then 3 and publish 3 then 95, leaving the live budget at 95.

// ErrPoolInUse refuses deleting a pool a connection, archived or not, still
// belongs to. Postgres's foreign key would refuse it and SQLite's schema has
// none, so the check is explicit.
var ErrPoolInUse = errors.New("exec: the connection pool still has connections")

// CreatePool adds a pool and returns its id.
func (e *Engine) CreatePool(ctx context.Context, token, name string, maxConns int, ip string) (int64, error) {
	ident, err := e.requireAdmin(ctx, token)
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	if err := validPool(name, maxConns); err != nil {
		return 0, err
	}
	defer e.lifecycleOp()()
	var id int64
	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		now := e.now().Unix()
		var terr error
		id, terr = e.store.Pools.On(tx).
			Set(meta.PoolName, name).Set(meta.PoolMaxConns, int64(maxConns)).
			Set(meta.PoolCreatedBy, ident.UserID()).
			Set(meta.PoolCreatedAt, now).Set(meta.PoolUpdatedAt, now).
			Insert()
		if terr != nil {
			if errors.Is(terr, dao.ErrDuplicate) {
				return fmt.Errorf("%w: a pool named %q already exists", ErrInvalidPool, name)
			}
			return terr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "pool_created",
			fmt.Sprintf("pool %d %q: max_conns %d", id, name, maxConns))
	})
	if err != nil {
		return 0, err
	}
	if err := e.pools.setPool(&meta.ConnectionPool{ID: id, Name: name, MaxConns: int64(maxConns)}); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdatePool changes a pool's max. It closes nothing: a lower max drains by
// attrition, and a raise serves waiters on both shares at once. No pgxpool is
// rebuilt, since none of their own caps binds (pgPoolLimits).
func (e *Engine) UpdatePool(ctx context.Context, token string, poolID int64, maxConns int, ip string) error {
	ident, err := e.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	defer e.lifecycleOp()()
	cur, err := e.store.Pools.OnCtx(ctx).With(meta.PoolID, poolID).Get()
	if err != nil {
		return poolNotFound(poolID, err)
	}
	if err := validPool(cur.Name, maxConns); err != nil {
		return err
	}
	if int64(maxConns) == cur.MaxConns {
		return nil
	}
	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if uerr := e.store.Pools.On(tx).With(meta.PoolID, poolID).
			Set(meta.PoolMaxConns, int64(maxConns)).
			Set(meta.PoolUpdatedAt, e.now().Unix()).Update(); uerr != nil {
			return uerr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "pool_updated",
			fmt.Sprintf("pool %d %q: max_conns %d -> %d", poolID, cur.Name, cur.MaxConns, maxConns))
	})
	if err != nil {
		return err
	}
	e.atLifecycleStep("pool.update committed")
	cur.MaxConns = int64(maxConns)
	if err := e.pools.setPool(cur); err != nil {
		return err
	}
	e.sessions.serve()
	return nil
}

// DeletePool removes a pool. The default pool, and any pool a connection
// (archived included) still belongs to, are refused.
func (e *Engine) DeletePool(ctx context.Context, token string, poolID int64, ip string) error {
	ident, err := e.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if poolID == meta.DefaultPoolID {
		return fmt.Errorf("%w: the default pool can't be deleted", ErrInvalidPool)
	}
	defer e.lifecycleOp()()
	cur, err := e.store.Pools.OnCtx(ctx).With(meta.PoolID, poolID).Get()
	if err != nil {
		return poolNotFound(poolID, err)
	}
	n, err := e.store.Connections.OnCtx(ctx).With(meta.ConnPoolID, poolID).Count()
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: pool %q has %d connection(s), archived ones included; move them first",
			ErrPoolInUse, cur.Name, n)
	}
	e.atLifecycleStep("pool.delete counted")
	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if derr := e.store.Pools.On(tx).With(meta.PoolID, poolID).Delete(); derr != nil {
			return derr
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "pool_deleted",
			fmt.Sprintf("pool %d %q", poolID, cur.Name))
	})
	if err != nil {
		return err
	}
	e.pools.dropPool(poolID)
	return nil
}

// ListPools reads every pool's live ledger. Authenticated, not admin-only: the
// budgets are what a session is held to, and reading them is how a refusal
// gets explained.
func (e *Engine) ListPools(ctx context.Context, token string) ([]PoolSnapshot, error) {
	if _, err := e.auth.ValidateToken(ctx, token); err != nil {
		return nil, err
	}
	return e.pools.snapshot(), nil
}

// SetConnectionPool moves a connection to another pool. It ends every session
// on the connection, as a profile downgrade does, because a live socket can't
// change ledgers.
//
// THE FENCE IS HELD ACROSS EVERY STEP, and it is this move's alone
// (lifecycleOp). closeSessionsFor marks the connection
// draining and detaches its pgxpool in one step under e.mu, so nothing can be
// admitted, and no pgxpool built on the old ledger, until clearDraining; it
// then ends the sessions and closes the pgxpool within poolCloseTimeout.
// Anyone turned away meanwhile is told the connection is being changed.
//
// Accepted: a socket the bounded close outlives stays charged to the old
// pool's ledger, which granted it, until it closes.
func (e *Engine) SetConnectionPool(ctx context.Context, token string, connID, poolID int64, ip string) error {
	ident, err := e.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	defer e.lifecycleOp()()
	row, err := e.liveConnection(ctx, connID)
	if err != nil {
		return err
	}
	if !inLedger(row) {
		return fmt.Errorf("%w: %s connections are outside every pool", ErrInvalidPool, row.Engine)
	}
	pool, err := e.store.Pools.OnCtx(ctx).With(meta.PoolID, poolID).Get()
	if err != nil {
		return poolNotFound(poolID, err)
	}
	if row.PoolID == poolID {
		return nil
	}

	e.closeSessionsFor(ctx, connID, ip, "connection-pool-changed", drainChanged)
	// A failed write leaves the connection where it was, and usable.
	defer e.sessions.clearDraining(connID)
	e.atLifecycleStep("conn.set_pool drained")

	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if uerr := e.store.Connections.On(tx).With(meta.ConnID, connID).
			Set(meta.ConnPoolID, poolID).
			Set(meta.ConnUpdatedAt, e.now().Unix()).Update(); uerr != nil {
			return uerr
		}
		return e.auth.AuditTxConn(tx, connID, ident.UserID(), ip, "connection_pool_set",
			fmt.Sprintf("%s: pool %d -> %d %q", row.Name, row.PoolID, poolID, pool.Name))
	})
	if err != nil {
		return err
	}
	row.PoolID = poolID
	// The next pgxpool build and the next lease both see the new pool.
	e.pools.setPoolOf(row)
	return nil
}

// lifecycleOp starts one lifecycle operation and returns its end: an admin
// write to pool budgets, pool membership or a connection's drain mark.
// Operations run one at a time, so each publishes what it committed, and the
// drain mark a write sets is the one it clears: one connection's two moves, or
// a move and a delete, would otherwise share a single mark and the first to
// finish would lift the other's fence.
//
// Lock order: lifecycle → e.mu → registry.mu → poolSet.mu. It is held over
// store I/O and session teardown, which is why nothing a statement takes ever
// waits for it.
func (e *Engine) lifecycleOp() (end func()) {
	// TryLock first only so the race-window hook can say which way the
	// acquisition went, before a writer that found the guard held waits.
	if e.lifecycle.TryLock() {
		e.atLifecycleStep(lifecycleAcquired)
		return e.lifecycle.Unlock
	}
	e.atLifecycleStep(lifecycleBlocked)
	e.lifecycle.Lock()
	return e.lifecycle.Unlock
}

// The steps lifecycleOp reports: the guard was free, or it was held and the
// writer is about to wait.
const (
	lifecycleAcquired = "lifecycle acquired"
	lifecycleBlocked  = "lifecycle blocked"
)

// atLifecycleStep marks a named step of a lifecycle operation, or of its
// acquisition, for the race-window hook. Nothing in production.
func (e *Engine) atLifecycleStep(step string) {
	if h := e.hookInLifecycleOp; h != nil {
		h(step)
	}
}

// requireAdmin is the authority check every pool write shares.
func (e *Engine) requireAdmin(ctx context.Context, token string) (auth.Identity, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return ident, err
	}
	if ident.Role() != meta.RoleAdmin {
		return ident, auth.ErrDenied
	}
	return ident, nil
}

// validPool is the ONE check of a pool's name and max.
func validPool(name string, maxConns int) error {
	if name == "" {
		return fmt.Errorf("%w: a pool needs a name", ErrInvalidPool)
	}
	if int64(maxConns) < meta.MinPoolConns || int64(maxConns) > meta.MaxPoolConns {
		return fmt.Errorf("%w: max_conns %d is outside %d..%d: a pool needs one socket each for the "+
			"control lane, the headroom share and the front-door share", ErrInvalidPool, maxConns,
			meta.MinPoolConns, meta.MaxPoolConns)
	}
	return nil
}

func poolNotFound(poolID int64, err error) error {
	if errors.Is(err, dao.ErrNoRows) {
		return fmt.Errorf("%w: no pool with id %d", ErrInvalidPool, poolID)
	}
	return err
}
