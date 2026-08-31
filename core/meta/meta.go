package meta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"

	"github.com/yongjohnlee80/autodb/core/config"
)

// Store is the opened meta-store: one dao connection, migrated to the
// current schema, with one immutable dao Schema per entity. Higher core
// layers (identity/authz in M3, execution in M4) build on these schemas.
type Store struct {
	conn   dao.DataConn
	engine string

	Users          *dao.Schema[*User, UserField, Sort, int64]
	Connections    *dao.Schema[*Connection, ConnField, Sort, int64]
	Workspaces     *dao.Schema[*Workspace, WorkspaceField, Sort, int64]
	WorkspaceConns *dao.Schema[*WorkspaceConn, WsConnField, Sort, int64]
	Grants         *dao.Schema[*Grant, GrantField, Sort, int64]
	Sessions       *dao.Schema[*Session, SessionField, Sort, int64]
	History        *dao.Schema[*HistoryEntry, HistoryField, Sort, int64]
	Audit          *dao.Schema[*AuditEntry, AuditField, Sort, int64]
	TxOutcomes     *dao.Schema[*TxOutcome, TxOutcomeField, Sort, int64]
	TxPending      *dao.Schema[*TxPending, TxPendingField, Sort, int64]
	AllowedIPs     *dao.Schema[*AllowedIP, AllowedIPField, Sort, int64]
	KV             *dao.Schema[*MetaKV, MetaKVField, Sort, string]
}

// Open opens the configured meta-store engine, runs pending migrations, and
// builds the entity schemas (ADR-0053 §2).
func Open(ctx context.Context, mcfg config.Meta) (*Store, error) {
	var (
		conn dao.DataConn
		err  error
	)
	switch mcfg.Engine {
	case "sqlite":
		conn, err = openSqlite(ctx, mcfg.Path)
	case "postgres":
		conn, err = postgres.OpenNamed(ctx, "meta", mcfg.DSN)
	default:
		return nil, fmt.Errorf("meta: unknown engine %q", mcfg.Engine)
	}
	if err != nil {
		return nil, fmt.Errorf("meta: opening %s store: %w", mcfg.Engine, err)
	}

	if err := runMigrations(ctx, conn, mcfg.Engine); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &Store{
		conn:           conn,
		engine:         mcfg.Engine,
		Users:          newUsers(conn),
		Connections:    newConnections(conn),
		Workspaces:     newWorkspaces(conn),
		WorkspaceConns: newWorkspaceConns(conn),
		Grants:         newGrants(conn),
		Sessions:       newSessions(conn),
		History:        newHistory(conn),
		Audit:          newAudit(conn),
		TxOutcomes:     newTxOutcomes(conn),
		TxPending:      newTxPending(conn),
		AllowedIPs:     newAllowedIPs(conn),
		KV:             newKV(conn),
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
	if path == "" {
		p, err := config.DefaultMetaPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating meta dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	return sqlite.OpenNamed(ctx, "meta", dsn)
}

// Close releases the underlying pool.
func (s *Store) Close() error { return s.conn.Close() }

// Conn exposes the store's dao connection for transactions (dao.RunTx) and
// raw statements.
func (s *Store) Conn() dao.DataConn { return s.conn }

// Engine reports "sqlite" or "postgres".
func (s *Store) Engine() string { return s.engine }

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
