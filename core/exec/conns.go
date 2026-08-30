package exec

import (
	"context"
	"errors"
	"fmt"

	"database/sql"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/mysql"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Driver open seams. Production uses golib's own functions; a test replaces
// them to observe what these branches actually pass.
//
// They exist because the pool bounds were "tested" by calling the option
// builders directly, which proved only that the builders work — the mysql and
// sqlite branches could be stripped of their bounds and every package stayed
// green. A driver call site that no test crosses is a call site that can lose
// an argument silently, and unbounded database/sql pools against a live
// production target is exactly the failure ADR-0074 §1a exists to prevent.
var (
	openPostgres = postgres.OpenNamed
	openMySQL    = mysql.OpenNamed
	openSQLite   = sqlite.OpenNamed
)

// target returns the cached dao connection for connID, opening it on first
// use: decrypt the identity-bound DSN (ErrLocked before any passphrase
// login), open the engine's driver, and probe with SELECT 1.
func (e *Engine) target(ctx context.Context, connID int64, row *meta.Connection) (dao.DataConn, error) {
	// A connection being deleted must not have its pool handed out or
	// RECREATED (ADR-0074 §1's ordering for conn.delete, applied at the one
	// place a pool is actually obtained).
	//
	// The check and the pool decision have to be ONE step. They were two:
	// the drain was checked, the lock dropped, e.mu taken, and a cached pool
	// returned or a new one opened — so a delete landing in between was
	// simply not seen, and stateless work carried on against a connection
	// the operator was removing.
	//
	// Making them one step means holding e.mu across the check, and the
	// driver's network open must NOT happen under it: one slow target would
	// stall every other connection in the engine. So the open is RESERVED
	// under the lock, performed outside it, and the drain re-checked before
	// the result is published. A delete arriving during the open loses the
	// pool rather than inheriting it.
	//
	// Lock order is engine.mu → registry.mu → session.mu. isDraining takes
	// the registry's lock, and no registry method reaches back for e.mu.
	for {
		e.mu.Lock()
		if e.sessions.isDraining(connID) {
			e.mu.Unlock()
			return nil, fmt.Errorf("%w: connection %d", ErrConnectionDraining, connID)
		}
		if h := e.hookAfterDrainCheck; h != nil {
			// Inside the window: the drain has been checked and no pool has
			// been handed out or published yet. The engine lock is still
			// held, which is precisely the property under test — a competing
			// delete must not be able to complete here.
			h()
		}
		if c, ok := e.conns[connID]; ok {
			e.mu.Unlock()
			return c, nil
		}
		if opening, ok := e.opening[connID]; ok {
			// Another caller is already opening this one. Wait for it rather
			// than opening a second pool that would immediately be orphaned
			// by whichever publish lost.
			e.mu.Unlock()
			select {
			case <-opening:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		reserved := make(chan struct{})
		e.opening[connID] = reserved
		e.mu.Unlock()

		conn, err := e.openTarget(ctx, connID, row)

		e.mu.Lock()
		delete(e.opening, connID)
		close(reserved)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		// The re-check is the whole point of the reservation: a delete that
		// began while the driver was connecting must win, and the pool it
		// did not know about must be closed rather than published.
		if e.sessions.isDraining(connID) {
			e.mu.Unlock()
			_ = conn.Close()
			return nil, fmt.Errorf("%w: connection %d", ErrConnectionDraining, connID)
		}
		e.conns[connID] = conn
		e.mu.Unlock()
		return conn, nil
	}
}

// openTarget performs the driver work for one connection, holding no engine
// lock: decrypt the identity-bound DSN (ErrLocked before any passphrase
// login), open the engine's driver, and verify it.
func (e *Engine) openTarget(ctx context.Context, connID int64, row *meta.Connection) (dao.DataConn, error) {
	dsn, err := e.auth.DecryptSecret(row.DSNEnc, connID)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("target-%d", connID)
	var conn dao.DataConn
	switch row.Engine {
	case "postgres":
		// Checkout-time grammar verification via pgxpool's PrepareConn
		// hook: the session's parsing mode is verified before EVERY
		// acquisition, so neither a fresh incompatible connection nor a
		// session mutated after pooling (set_config through a verb-level
		// read) can serve a statement. Autocommit is preserved, keeping
		// transaction-prohibited DDL executable (ADR-0055 rev 5).
		conn, err = openPostgres(ctx, name, string(dsn),
			pgPrepareConnVerify(), e.pgPoolLimits(row))
	case "mysql":
		conn, err = openMySQL(ctx, name, string(dsn), mysql.Option(e.sqlPoolLimits(row)))
	case "sqlite":
		conn, err = openSQLite(ctx, name, string(dsn), sqlite.Option(e.sqlPoolLimits(row)))
	default:
		return nil, fmt.Errorf("exec: connection %d has unknown engine %q", connID, row.Engine)
	}
	if err != nil {
		return nil, fmt.Errorf("exec: opening connection %q: %w", row.Name, err)
	}
	// Re-validate the stored DSN (driver parsers, not substrings) and probe
	// one session's parsing mode for a fast, clear failure at first use.
	// This is a BELT check only: the authoritative grammar verification runs
	// per physical session at execution time (verifyGrammarQ on the pinned
	// TxConn in the engine's run path — lector M4 r3).
	if verr := ValidateDSN(row.Engine, string(dsn)); verr != nil {
		_ = conn.Close()
		return nil, verr
	}
	if verr := verifyGrammarQ(ctx, conn, row.Engine); verr != nil {
		_ = conn.Close()
		return nil, verr
	}
	return conn, nil
}

// pgPoolLimits applies the target-pool bounds (ADR-0074 §1a).
//
// A pinned transaction holds a physical connection for as long as its session
// keeps it open, so an unbounded pool lets a few callers with open
// transactions consume a production database's entire connection budget —
// and the first thing to fail is somebody else's application. The bound is
// the point; the retirement settings are what keep a server-side change from
// requiring a daemon restart.
//
// The per-connection value is a REQUEST, capped here rather than at the point
// it was stored. That ordering matters: lowering the install-wide ceiling
// immediately binds every connection that had asked for more, with no rows to
// rewrite and no window where a stale request outranks the current policy.
func (e *Engine) poolLimitsFor(row *meta.Connection) (int, time.Duration, time.Duration) {
	max := e.poolMaxConns
	if row != nil && row.PoolMaxConns > 0 && int(row.PoolMaxConns) < max {
		max = int(row.PoolMaxConns)
	}
	return max, e.poolMaxConnIdleTime, e.poolMaxConnLifetime
}

// sqlPoolLimits applies the same bounds to the database/sql drivers.
//
// These needed them MORE than pgxpool did, not less. pgxpool at least reaps
// idle connections on its own; database/sql keeps idle physical connections
// to the target indefinitely and has no default cap on open ones at all, so
// mysql and sqlite targets were the unbounded case while only postgres was
// wired. ADR-0074 §1a says all three drivers, and it says so for this reason.
func (e *Engine) sqlPoolLimits(row *meta.Connection) func(*sql.DB) {
	max, idle, lifetime := e.poolLimitsFor(row)
	return func(db *sql.DB) {
		if max > 0 {
			db.SetMaxOpenConns(max)
		}
		if idle > 0 {
			db.SetConnMaxIdleTime(idle)
		}
		if lifetime > 0 {
			db.SetConnMaxLifetime(lifetime)
		}
	}
}

func (e *Engine) pgPoolLimits(row *meta.Connection) postgres.Option {
	max, idle, lifetime := e.poolLimitsFor(row)
	return func(cfg *pgxpool.Config) {
		if max > 0 {
			cfg.MaxConns = int32(max)
		}
		if idle > 0 {
			cfg.MaxConnIdleTime = idle
		}
		if lifetime > 0 {
			cfg.MaxConnLifetime = lifetime
		}
	}
}

// closeTarget drops a cached connection (used on delete).
func (e *Engine) closeTarget(connID int64) {
	e.mu.Lock()
	c, ok := e.conns[connID]
	delete(e.conns, connID)
	e.mu.Unlock()
	// Closed OUTSIDE e.mu. A pool's Close waits for every acquired
	// connection, so closing under the engine's lock lets one target with a
	// held connection stall every other connection in the engine.
	if ok {
		_ = c.Close()
	}
}

// beginDraining marks a connection draining and detaches its pool as ONE
// step, returning the sessions to close and the pool to shut down.
//
// The atomicity is the fix. Marking and pool lookup were separate operations
// under separate locks, so target() could check the drain, find it clear, and
// hand out (or open) a pool for a connection whose delete had already begun.
// Both now happen under the engine's lock, which target() holds across its
// own check, so the two orders are the only two possible ones: either the
// pool is obtained before the drain and closed by the delete, or the drain is
// seen and the request refused.
//
// The pool is DETACHED here but not closed here — its Close waits for every
// acquired connection, and the sessions holding those connections have not
// been torn down yet. The caller closes it after they have.
//
// Lock order is engine.mu → registry.mu, matching target().
func (e *Engine) beginDraining(connID int64) ([]*session, dao.DataConn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	drained := e.sessions.setDraining(connID)
	pool, ok := e.conns[connID]
	if !ok {
		return drained, nil
	}
	delete(e.conns, connID)
	return drained, pool
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
	if name == "" || dsn == "" {
		return 0, errors.New("exec: connection name and dsn must not be empty")
	}
	// Reject DSNs whose options would desynchronize the classifier from the
	// target's grammar (multi-statement, sql_mode, interpolation).
	if err := ValidateDSN(engineName, dsn); err != nil {
		return 0, err
	}
	if !e.auth.Unlocked() {
		return 0, auth.ErrLocked
	}
	var id int64
	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
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
		// Ownership grant: token-proven actor, creator relationship verified
		// against the inserted row, role capped at editor (lector M3 r2
		// must-fix #1 + the auto-grant policy ruling).
		creator, terr := e.auth.GrantCreatorTx(tx, token, id)
		if terr != nil {
			return fmt.Errorf("exec: granting creator ownership: %w", terr)
		}
		return e.auth.AuditTx(tx, creator.UserID(), ip, "connection_created",
			fmt.Sprintf("%s (%s), creator granted ownership", name, engineName))
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
	// Order matters and is the ADR-0074 §1 sequence: mark the connection
	// DRAINING and close its sessions FIRST, then drop the pool. Closing the
	// pool first would pull it out from under a session that still believed
	// it could run, and marking after closing would let a session be opened
	// onto a connection already being deleted.
	//
	// closeSessionsFor now owns the whole sequence — the mark and the pool
	// detach are one atomic step, so nothing can obtain a pool in between —
	// and it closes the detached pool once the sessions are gone.
	e.closeSessionsFor(ctx, connID, ip, "connection-deleted")

	err = dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if err := e.store.Connections.On(tx).With(meta.ConnID, connID).Delete(); err != nil {
			if errors.Is(err, dao.ErrForeignKey) {
				return fmt.Errorf("exec: connection %q has recorded history and cannot be deleted: %w", row.Name, err)
			}
			return err
		}
		return e.auth.AuditTx(tx, ident.UserID(), ip, "connection_deleted", row.Name)
	})
	if err != nil {
		// The row survives, so the connection must become usable again —
		// otherwise a delete that failed on a foreign key would leave a live
		// connection permanently unopenable with nothing saying why.
		e.sessions.clearDraining(connID)
	}
	return err
}

// TestConnection authorizes read access and probes the target with SELECT 1.
// Authorization precedes the row fetch so an ungranted caller learns nothing
// about the connection's existence (lector M4 must-fix #6).
func (e *Engine) TestConnection(ctx context.Context, token string, connID int64, ip string) error {
	ident, err := e.auth.Authorize(ctx, token, connID, auth.ActionRead)
	if err != nil {
		return err
	}
	row, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return auth.ErrDenied
	}
	if err != nil {
		return err
	}
	conn, err := e.target(ctx, connID, row)
	if err != nil {
		// Connection failures are security-relevant signal (credential
		// rotation, tampering) — audit them (lector M4 should-fix).
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "conn_test_failed",
			fmt.Sprintf("conn %d: %v", connID, err)); aerr != nil {
			return aerr
		}
		return err
	}
	rows, err := conn.QueryContext(ctx, "SELECT 1")
	if err != nil {
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "conn_test_failed",
			fmt.Sprintf("conn %d probe: %v", connID, err)); aerr != nil {
			return aerr
		}
		return fmt.Errorf("exec: probe failed: %w", err)
	}
	if cerr := rows.Close(); cerr != nil {
		// A close failure is a probe failure — audit it like one (lector
		// M4 r3 amendment: no unaudited exit path from TestConnection).
		if aerr := e.auth.Audit(ctx, ident.UserID(), ip, "conn_test_failed",
			fmt.Sprintf("conn %d close: %v", connID, cerr)); aerr != nil {
			return aerr
		}
		return cerr
	}
	// A successful test is security-relevant too (who verified reachability,
	// from where) — audit it (lector M4 r2 amendment).
	return e.auth.Audit(ctx, ident.UserID(), ip, "conn_test_ok", fmt.Sprintf("conn %d", connID))
}
