package meta

import (
	"context"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/dao"
)

// migration is one schema version: per-engine DDL applied atomically.
type migration struct {
	Version  int
	SQLite   []string
	Postgres []string
}

// migrations is the ordered, append-only schema history (ADR-0053 §3).
var migrations = []migration{
	{Version: 1, SQLite: ddlV1SQLite, Postgres: ddlV1Postgres},
}

// runMigrations creates the ledger, checks the downgrade guard, and applies
// pending versions in order — each version inside one driver transaction
// (both engines support transactional DDL).
func runMigrations(ctx context.Context, conn dao.DataConn, engine string) error {
	const ledger = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at BIGINT NOT NULL)`
	if _, err := conn.ExecContext(ctx, ledger); err != nil {
		return fmt.Errorf("meta: creating schema_migrations: %w", err)
	}

	cur64, err := currentVersion(ctx, conn)
	if err != nil {
		return err
	}
	cur := int(cur64)
	latest := migrations[len(migrations)-1].Version
	if cur > latest {
		return fmt.Errorf("meta: store schema version %d is newer than this binary's %d — refusing to open (downgrade guard)", cur, latest)
	}

	for _, m := range migrations {
		if m.Version <= cur {
			continue
		}
		stmts := m.SQLite
		if engine == "postgres" {
			stmts = m.Postgres
		}
		if err := applyOne(ctx, conn, m.Version, stmts); err != nil {
			return fmt.Errorf("meta: migration %d: %w", m.Version, err)
		}
	}
	return nil
}

func currentVersion(ctx context.Context, conn dao.DataConn) (int64, error) {
	rows, err := conn.QueryContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("meta: reading schema version: %w", err)
	}
	defer rows.Close()
	var v int64
	if !rows.Next() {
		return 0, rows.Err()
	}
	if err := rows.Scan(&v); err != nil {
		return 0, err
	}
	return v, rows.Err()
}

func applyOne(ctx context.Context, conn dao.DataConn, version int, stmts []string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying %q: %w", firstLine(stmt), err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (`+
			conn.Dialect().Placeholder(1)+`, `+conn.Dialect().Placeholder(2)+`)`,
		version, time.Now().Unix()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("recording version: %w", err)
	}
	return tx.Commit()
}

// firstLine trims a DDL statement to its first line for error messages.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// --- v1 DDL (ADR-0053 §2) ------------------------------------------------------
//
// Portability rules: int64 autoincrement ids, unix-second BIGINT timestamps,
// INTEGER 0/1 flags, TEXT enums with CHECK constraints.

var ddlV1SQLite = []string{
	`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
		pass_hash BLOB NOT NULL,
		disabled INTEGER NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL)`,
	`CREATE TABLE connections (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		engine TEXT NOT NULL CHECK (engine IN ('postgres','mysql','sqlite')),
		dsn_enc BLOB NOT NULL,
		created_by INTEGER NOT NULL REFERENCES users(id),
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL)`,
	`CREATE TABLE workspaces (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE workspace_connections (
		id INTEGER PRIMARY KEY,
		workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		UNIQUE (workspace_id, connection_id))`,
	`CREATE TABLE grants (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
		granted_by INTEGER NOT NULL REFERENCES users(id),
		created_at BIGINT NOT NULL,
		UNIQUE (user_id, connection_id))`,
	`CREATE TABLE sessions (
		id INTEGER PRIMARY KEY,
		token_hash BLOB NOT NULL UNIQUE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		ip TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE script_history (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id),
		connection_id INTEGER NOT NULL REFERENCES connections(id),
		ip TEXT NOT NULL,
		script TEXT NOT NULL,
		started_at BIGINT NOT NULL,
		duration_ms BIGINT NOT NULL DEFAULT 0,
		row_count BIGINT NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '')`,
	// audit_log has NO foreign keys by design: auditing must never fail, and
	// user_id 0 records pre-auth events (ADR-0053 §2).
	`CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL DEFAULT 0,
		ip TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE ip_allowlist (
		id INTEGER PRIMARY KEY,
		cidr TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '',
		created_by INTEGER NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE store_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL)`,
	`CREATE INDEX idx_history_user ON script_history(user_id, started_at)`,
	`CREATE INDEX idx_audit_created ON audit_log(created_at)`,
	`CREATE INDEX idx_sessions_user ON sessions(user_id)`,
}

var ddlV1Postgres = []string{
	`CREATE TABLE users (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
		pass_hash BYTEA NOT NULL,
		disabled INTEGER NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL)`,
	`CREATE TABLE connections (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		engine TEXT NOT NULL CHECK (engine IN ('postgres','mysql','sqlite')),
		dsn_enc BYTEA NOT NULL,
		created_by BIGINT NOT NULL REFERENCES users(id),
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL)`,
	`CREATE TABLE workspaces (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE workspace_connections (
		id BIGSERIAL PRIMARY KEY,
		workspace_id BIGINT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		connection_id BIGINT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		UNIQUE (workspace_id, connection_id))`,
	`CREATE TABLE grants (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		connection_id BIGINT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
		granted_by BIGINT NOT NULL REFERENCES users(id),
		created_at BIGINT NOT NULL,
		UNIQUE (user_id, connection_id))`,
	`CREATE TABLE sessions (
		id BIGSERIAL PRIMARY KEY,
		token_hash BYTEA NOT NULL UNIQUE,
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		ip TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE script_history (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL REFERENCES users(id),
		connection_id BIGINT NOT NULL REFERENCES connections(id),
		ip TEXT NOT NULL,
		script TEXT NOT NULL,
		started_at BIGINT NOT NULL,
		duration_ms BIGINT NOT NULL DEFAULT 0,
		row_count BIGINT NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE audit_log (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL DEFAULT 0,
		ip TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE ip_allowlist (
		id BIGSERIAL PRIMARY KEY,
		cidr TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '',
		created_by BIGINT NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL)`,
	`CREATE TABLE store_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL)`,
	`CREATE INDEX idx_history_user ON script_history(user_id, started_at)`,
	`CREATE INDEX idx_audit_created ON audit_log(created_at)`,
	`CREATE INDEX idx_sessions_user ON sessions(user_id)`,
}
