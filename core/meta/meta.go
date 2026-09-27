package meta

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"
)

// Store represents an active connection to autodb's management database, migrated to
// the current schema version and providing type-safe dao.Schema handles for all entities.
//
// Higher-level core packages (such as core/auth and core/exec) build directly upon
// these schemas. Store is safe for concurrent use across goroutines: the underlying
// database connection pool manages concurrent queries and transactions.
type Store struct {
	conn   dao.DataConn
	engine engine.Name

	Users          *dao.Schema[*User, UserField, Sort, int64]
	Connections    *dao.Schema[*Connection, ConnField, Sort, int64]
	Workspaces     *dao.Schema[*Workspace, WorkspaceField, Sort, int64]
	WorkspaceConns *dao.Schema[*WorkspaceConn, WsConnField, Sort, int64]
	Grants         *dao.Schema[*Grant, GrantField, Sort, int64]
	Sessions       *dao.Schema[*Session, SessionField, Sort, int64]
	PATs           *dao.Schema[*PAT, PATField, Sort, int64]
	History        *dao.Schema[*HistoryEntry, HistoryField, Sort, int64]
	Audit          *dao.Schema[*AuditEntry, AuditField, Sort, int64]
	TxOutcomes     *dao.Schema[*TxOutcome, TxOutcomeField, Sort, int64]
	TxPending      *dao.Schema[*TxPending, TxPendingField, Sort, int64]
	AllowedIPs     *dao.Schema[*AllowedIP, AllowedIPField, Sort, int64]
	UserIPs        *dao.Schema[*UserIP, UserIPField, Sort, int64]
	KV             *dao.Schema[*MetaKV, MetaKVField, Sort, string]
	Keyslots       *dao.Schema[*Keyslot, KeyslotField, Sort, string]

	// schemaWarnings are what Open's upgrade found (scripts.go).
	schemaWarnings []string
	// sqlitePath is a SQLite store's resolved file, "" for :memory: and
	// PostgreSQL: what a backup before a schema change copies (backup.go).
	sqlitePath string
	// startReport is what Open's upgrade did (backup.go).
	startReport StartReport
	// Test seams for a backup's name and its copy (backup.go); nil in use.
	hookBackupName func(base, first string) (string, error)
	hookVacuum     func(ctx context.Context, path string) error
}

// Open opens the configured meta-store engine, runs pending migrations, and
// builds the entity schemas.
// metaPoolBound sizes the meta store's own pool.
//
// The meta store is NOT a target pool and must not borrow the target pool's
// 2 x cores: that number is sized by how much USER traffic a target absorbs,
// while this one serves the daemon's own bookkeeping, whose concurrency the
// daemon sets. Sizing it by cores would buy nothing and spend postgres
// backends the target pools need.
//
// An explicit [meta] pool_max_conns wins. Otherwise a pool_max_conns already
// in the DSN is left alone — someone who wrote it there meant it — and only a
// DSN that says nothing gets the default.
func metaPoolBound(mcfg StoreConfig) postgres.Option {
	// The SAME decision the validator made — one function, two callers
	// When these were decided separately, a DSN-level
	// pool_max_conns=1 satisfied validation (which only looked at the TOML
	// field) and then won at connect time, producing exactly the
	// one-connection pool the floor exists to prevent.
	n := mcfg.StorePoolMaxConns()
	return func(c *pgxpool.Config) { c.MaxConns = int32(n) }
}

// Open connects and brings the schema up to date.
func Open(ctx context.Context, mcfg StoreConfig) (*Store, error) {
	s, err := OpenNoMigrate(ctx, mcfg)
	if err != nil {
		return nil, err
	}
	// A SQLite store is copied before its schema changes (backup.go). The
	// copy only reads the store, so if it fails nothing has changed.
	pending, backup, err := s.backupBeforeChange(ctx)
	if err != nil {
		_ = s.conn.Close()
		return nil, err
	}
	warnings, err := runMigrations(ctx, s.conn, mcfg.StoreEngine())
	if err != nil {
		_ = s.conn.Close()
		return nil, err
	}
	s.schemaWarnings = warnings
	s.startReport = StartReport{Applied: pending, Backup: backup}
	if backup != "" {
		s.rotateBackups()
	}
	return s, nil
}

// OpenNoMigrate connects WITHOUT touching the schema.
//
// It exists for one caller and one reason:
// the migration CLI must prove no daemon is serving BEFORE it mutates
// anything — and running migrations IS a mutation. Open cannot serve that,
// because it migrates before it returns, so a CLI built on Open would already
// have changed the destination's schema by the time it was in a position to
// take the lease and find out it should not have.
//
// The returned Store is safe for the lease and for reading `schema_migrations`,
// and nothing else should assume its schema is current. Everything that serves
// requests uses Open.
func OpenNoMigrate(ctx context.Context, mcfg StoreConfig) (*Store, error) {
	var (
		conn       dao.DataConn
		err        error
		sqlitePath string
	)
	switch mcfg.StoreEngine() {
	case engine.SQLite:
		if sqlitePath, err = resolveSqlitePath(mcfg.StorePath()); err != nil {
			return nil, err
		}
		conn, err = openSqlite(ctx, sqlitePath)
	case engine.Postgres:
		conn, err = postgres.OpenNamed(ctx, "meta", mcfg.StoreDSN(), metaPoolBound(mcfg))
	default:
		return nil, fmt.Errorf("meta: unknown engine %q", mcfg.StoreEngine())
	}
	if err != nil {
		return nil, fmt.Errorf("meta: opening %s store: %w", mcfg.StoreEngine(), err)
	}

	if sqlitePath == ":memory:" {
		sqlitePath = ""
	}
	return &Store{
		conn:           conn,
		engine:         mcfg.StoreEngine(),
		sqlitePath:     sqlitePath,
		Users:          newUsers(conn),
		Connections:    newConnections(conn),
		Workspaces:     newWorkspaces(conn),
		WorkspaceConns: newWorkspaceConns(conn),
		Grants:         newGrants(conn),
		Sessions:       newSessions(conn),
		PATs:           newPATs(conn),
		History:        newHistory(conn),
		Audit:          newAudit(conn),
		TxOutcomes:     newTxOutcomes(conn),
		TxPending:      newTxPending(conn),
		AllowedIPs:     newAllowedIPs(conn),
		UserIPs:        newUserIPs(conn),
		KV:             newKV(conn),
		Keyslots:       newKeyslots(conn),
	}, nil
}

// openSqlite resolves the sqlite path (default: $XDG_DATA_HOME/autodb/meta.db),
// creates the parent directory 0700, and opens with WAL + busy_timeout +
// foreign_keys pragmas. ":memory:" is supported for tests (single-connection).
func openSqlite(ctx context.Context, path string) (dao.DataConn, error) {
	if path == ":memory:" {
		return sqlite.OpenNamed(ctx, "meta", "file::memory:?_pragma=foreign_keys(1)",
			sqlite.MaxOpenConns(1))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating meta dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	return sqlite.OpenNamed(ctx, "meta", dsn)
}

// resolveSqlitePath is the file a SQLite store lives in: path, or the default
// ($XDG_DATA_HOME/autodb/meta.db) when it is empty. ":memory:" stays itself.
func resolveSqlitePath(path string) (string, error) {
	if path == "" {
		return DefaultPath()
	}
	return path, nil
}

// Close releases the underlying pool.
func (s *Store) Close() error { return s.conn.Close() }

// Conn exposes the store's dao connection for transactions (dao.RunTx) and
// raw statements.
func (s *Store) Conn() dao.DataConn { return s.conn }

// Engine reports "sqlite" or "postgres".
func (s *Store) Engine() engine.Name { return s.engine }

// GetMeta reads one store_meta key. Missing keys report ok == false, not an
// error.
func (s *Store) GetMeta(ctx context.Context, key string) (value string, ok bool, err error) {
	kv, err := s.KV.OnCtx(ctx).With(KVKey, key).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return kv.Value, true, nil
}

// SetMeta writes one store_meta key (insert-or-update).
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.KV.OnCtx(ctx).Set(KVKey, key).Set(KVValue, value).Upsert()
}
