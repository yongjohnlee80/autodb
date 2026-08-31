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
	// v2 (ADR-0054 §6): per-user master-key keyslot. NOT NULL with an empty
	// default so v1 stores upgrade; auth code enforces non-empty at creation.
	{
		Version:  2,
		SQLite:   []string{`ALTER TABLE users ADD COLUMN mk_wrapped BLOB NOT NULL DEFAULT x''`},
		Postgres: []string{`ALTER TABLE users ADD COLUMN mk_wrapped BYTEA NOT NULL DEFAULT '\x'::bytea`},
	},
	// v3 (ADR-0074 §2, Amendment 2 C2): the per-connection capability
	// profile and the debug flag.
	//
	// Both default to today's behaviour, which is the safe direction and the
	// reason a migration can be applied to a live store without a decision:
	// every existing connection keeps v1compat, so sessions are opt-in per
	// connection rather than something a schema upgrade turns on. debug
	// defaults off, so every connection keeps the 90-second idle bound
	// rather than the debug profile's 10 minutes.
	//
	// The 0/1 INTEGER flag matches users.disabled on both engines — the
	// house pattern here — rather than introducing a second convention for
	// booleans in one table.
	{
		Version: 3,
		SQLite: []string{
			`ALTER TABLE connections ADD COLUMN profile TEXT NOT NULL DEFAULT 'v1compat'`,
			`ALTER TABLE connections ADD COLUMN debug INTEGER NOT NULL DEFAULT 0`,
		},
		Postgres: []string{
			`ALTER TABLE connections ADD COLUMN profile TEXT NOT NULL DEFAULT 'v1compat'`,
			`ALTER TABLE connections ADD COLUMN debug INTEGER NOT NULL DEFAULT 0`,
		},
	},
	// v4 (ADR-0074 §1a): the per-connection pool bound.
	//
	// 0 means "take the install-wide value", which is why the column can be
	// added to a live store without a decision — every existing connection
	// keeps exactly the behaviour it had. A row may ask for FEWER connections
	// than the operator's ceiling and never more; the cap is applied when the
	// pool is opened rather than when the value is stored, so lowering the
	// install-wide number immediately binds every connection that had asked
	// for more, with no rows to rewrite.
	{
		Version: 4,
		SQLite: []string{
			`ALTER TABLE connections ADD COLUMN pool_max_conns INTEGER NOT NULL DEFAULT 0`,
		},
		Postgres: []string{
			`ALTER TABLE connections ADD COLUMN pool_max_conns INTEGER NOT NULL DEFAULT 0`,
		},
	},
	// v5 (ADR-0074 §7 rev 2 + Amendment 4): the transaction outcome log.
	//
	// APPEND-ONLY, and that is the whole point. The v1 model recorded an
	// outcome by UPDATEing script_history in place, overwriting "running"
	// with "ok" or "error" — which cannot express a progression, cannot
	// express an outcome that is not yet knowable, and destroys the earlier
	// state it overwrites. §7's machine needs all three, so outcomes get
	// their own table and script_history becomes a projection of it.
	//
	// One row per state transition, ordered by seq within a tx_id:
	//
	//   opened          -- at BEGIN, carrying the target's xid for recovery
	//   commit_started  -- appended BEFORE the target Commit call
	//   unknown_pending -- appended by a resolver that could not prove it
	//   committed | rolled_back | outcome_unresolvable   -- terminal
	//
	// §7: "an absent record after commit_started is interpreted as
	// unknown_pending", so the nonterminal state is inferable from absence
	// as well as recordable explicitly. Both are the same fact.
	//
	// target_xid is nullable because it only exists where the dialect has
	// one: Postgres txid_current(). Its absence IS the no-oracle condition
	// Amendment 4 A3 terminates as outcome_unresolvable(reason=no-oracle),
	// so the column not being set is load-bearing rather than incidental.
	//
	// No foreign keys, for the same reason audit_log has none (ADR-0053 §2):
	// the outcome trail must never fail to record because something else it
	// points at is gone. A deleted connection must not take the evidence of
	// what it did with it.
	//
	// TWO durable guards, because they enforce different invariants and
	// neither implies the other:
	//
	//   UNIQUE(tx_id, seq)              -- append-only. A writer that tries
	//                                      to rewrite a transition collides
	//                                      instead of silently succeeding.
	//   UNIQUE(tx_id) WHERE terminal    -- exactly-one-terminal. Two
	//                                      resolvers appending DIFFERENT
	//                                      seqs would both satisfy the first
	//                                      index while contradicting each
	//                                      other, so G2 needs its own.
	//
	// The second one lives in the store on purpose (lector Amendment-4 r0
	// MF3): the instance lease gives one PROCESS, not one goroutine, and the
	// periodic reconciler, the checkout trigger, the boundary handler and
	// the timeout reaper can all overlap inside it. An application-level
	// check-then-write cannot make exactly-one-terminal true across four
	// concurrent resolvers; a unique index can, and a loser sees a
	// constraint violation it must read as "another resolver got there
	// first" rather than as an error to retry into a duplicate.
	//
	// Partial indexes are supported by both engines (SQLite 3.8+, Postgres),
	// so this is one predicate rather than two dialect-specific mechanisms.
	{
		Version: 5,
		SQLite: []string{
			`CREATE TABLE tx_outcomes (
				id INTEGER PRIMARY KEY,
				tx_id TEXT NOT NULL,
				seq INTEGER NOT NULL,
				state TEXT NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				user_id INTEGER NOT NULL DEFAULT 0,
				connection_id INTEGER NOT NULL DEFAULT 0,
				history_id INTEGER NOT NULL DEFAULT 0,
				target_xid TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL)`,
			`CREATE UNIQUE INDEX idx_tx_outcomes_seq ON tx_outcomes(tx_id, seq)`,
			`CREATE UNIQUE INDEX idx_tx_outcomes_terminal ON tx_outcomes(tx_id) WHERE state IN ('committed','rolled_back','outcome_unresolvable')`,
			`CREATE INDEX idx_tx_outcomes_state ON tx_outcomes(state, created_at)`,
		},
		Postgres: []string{
			`CREATE TABLE tx_outcomes (
				id BIGSERIAL PRIMARY KEY,
				tx_id TEXT NOT NULL,
				seq BIGINT NOT NULL,
				state TEXT NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				user_id BIGINT NOT NULL DEFAULT 0,
				connection_id BIGINT NOT NULL DEFAULT 0,
				history_id BIGINT NOT NULL DEFAULT 0,
				target_xid TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL)`,
			`CREATE UNIQUE INDEX idx_tx_outcomes_seq ON tx_outcomes(tx_id, seq)`,
			`CREATE UNIQUE INDEX idx_tx_outcomes_terminal ON tx_outcomes(tx_id) WHERE state IN ('committed','rolled_back','outcome_unresolvable')`,
			`CREATE INDEX idx_tx_outcomes_state ON tx_outcomes(state, created_at)`,
		},
	},
	// v6 (ADR-0074 §7 rev 2): tx_id on the audit trail and on history.
	//
	// R3 already issues a tx_id and writes it into the free-text `detail`
	// string of its boundary events. A substring is not a correlation key:
	// it cannot be indexed, joined, or trusted against a format change. The
	// column is what lets an operator ask "everything that happened inside
	// this transaction" and get an answer.
	//
	// Empty string, not NULL, for statements outside any transaction — the
	// house pattern for "no value" in this schema, and it keeps every read
	// path free of null handling.
	{
		Version: 6,
		SQLite: []string{
			`ALTER TABLE audit_log ADD COLUMN tx_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE script_history ADD COLUMN tx_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX idx_audit_tx ON audit_log(tx_id)`,
		},
		Postgres: []string{
			`ALTER TABLE audit_log ADD COLUMN tx_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE script_history ADD COLUMN tx_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX idx_audit_tx ON audit_log(tx_id)`,
		},
	},
	// v7 adds the durable outcome QUEUE (ADR-0074 §7: "Opening a tx enqueues
	// a durable finalization-pending entry in core/meta keyed by tx_id …
	// resolved by whichever writer learns the truth first").
	//
	// The log alone cannot answer "what is still unresolved?" cheaply, and
	// the reason is structural rather than an indexing oversight: every
	// transaction keeps its `opened` row forever, and every committed one
	// keeps its `commit_started` too, so no predicate over STATES can
	// separate the pending from the settled. Any such query selects the
	// whole history (PR #20 r1 MF1).
	//
	// So the queue is a separate, SMALL table holding exactly the
	// unresolved: one row per transaction, inserted when it opens and
	// deleted — in the same store transaction as the terminal — when it
	// settles. It is a pure index into the log, carrying no state of its
	// own, so it cannot drift from the truth or become a second place where
	// an outcome is recorded.
	{
		Version: 7,
		SQLite: []string{
			`CREATE TABLE tx_pending (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				tx_id TEXT NOT NULL UNIQUE,
				connection_id BIGINT NOT NULL,
				created_at BIGINT NOT NULL)`,
			`CREATE INDEX idx_tx_pending_conn ON tx_pending(connection_id)`,
			// Backfill: everything already on disk without a terminal.
			// Bounded, runs once, and without it a store upgraded from v6
			// would silently lose track of its existing backlog.
			`INSERT INTO tx_pending (tx_id, connection_id, created_at)
				SELECT tx_id, MIN(connection_id), MIN(created_at) FROM tx_outcomes
				WHERE tx_id NOT IN (SELECT tx_id FROM tx_outcomes WHERE state IN ('committed','rolled_back','outcome_unresolvable'))
				GROUP BY tx_id`,
		},
		Postgres: []string{
			`CREATE TABLE tx_pending (
				id BIGSERIAL PRIMARY KEY,
				tx_id TEXT NOT NULL UNIQUE,
				connection_id BIGINT NOT NULL,
				created_at BIGINT NOT NULL)`,
			`CREATE INDEX idx_tx_pending_conn ON tx_pending(connection_id)`,
			`INSERT INTO tx_pending (tx_id, connection_id, created_at)
				SELECT tx_id, MIN(connection_id), MIN(created_at) FROM tx_outcomes
				WHERE tx_id NOT IN (SELECT tx_id FROM tx_outcomes WHERE state IN ('committed','rolled_back','outcome_unresolvable'))
				GROUP BY tx_id`,
		},
	},
	// v8 gives the queue the columns a FAIR page needs (PR #20 r2).
	//
	// A LIMIT with no ordering and no cursor revisits the same page forever,
	// so a resolvable entry sitting behind a screenful of live ones is never
	// reached — starvation, not slowness. Ordering needs a stable key, and
	// scoping the user-facing read before its limit needs the owner, which
	// the queue did not carry.
	{
		Version: 8,
		SQLite: []string{
			`ALTER TABLE tx_pending ADD COLUMN user_id BIGINT NOT NULL DEFAULT 0`,
			`UPDATE tx_pending SET user_id = COALESCE((
				SELECT o.user_id FROM tx_outcomes o
				WHERE o.tx_id = tx_pending.tx_id ORDER BY o.seq LIMIT 1), 0)`,
			`CREATE INDEX idx_tx_pending_order ON tx_pending(created_at, id)`,
			`CREATE INDEX idx_tx_pending_user ON tx_pending(user_id, created_at)`,
		},
		Postgres: []string{
			`ALTER TABLE tx_pending ADD COLUMN user_id BIGINT NOT NULL DEFAULT 0`,
			`UPDATE tx_pending SET user_id = COALESCE((
				SELECT o.user_id FROM tx_outcomes o
				WHERE o.tx_id = tx_pending.tx_id ORDER BY o.seq LIMIT 1), 0)`,
			`CREATE INDEX idx_tx_pending_order ON tx_pending(created_at, id)`,
			`CREATE INDEX idx_tx_pending_user ON tx_pending(user_id, created_at)`,
		},
	},
	// v9 (ADR-0075 §4): the per-user layer of the front door's two-layer IP
	// model. UNIQUE(user_id, cidr) makes re-adding idempotent-by-refusal;
	// ON DELETE CASCADE because a removed user's allowlist rows authorize
	// nobody and must not linger as orphans. (Authored as a provisional v5;
	// renumbered to v9 when R4's v5-v8 merged first, per the coordination
	// note both PRs carried.)
	{
		Version: 9,
		SQLite: []string{
			`CREATE TABLE user_ip_allowlist (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		cidr TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		UNIQUE(user_id, cidr))`,
		},
		Postgres: []string{
			`CREATE TABLE user_ip_allowlist (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		cidr TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		UNIQUE(user_id, cidr))`,
		},
	},
	// v10 records WHEN a settled progression was collapsed to its tombstone
	// (ADR-0079 §3 / P4).
	//
	// Retention for the outcome log COLLAPSES rather than deletes: the
	// intermediate transitions of a settled transaction go, the terminal
	// stays. That is not a storage detail, it is the whole mechanism — the
	// read API's ErrNoSuchTx means "no transaction was started", and its
	// truthfulness rests on the write-ahead ordering in beginTx (zero rows
	// PROVES nothing started). Deleting settled rows would break that proof
	// and a committed transaction would begin answering "no such
	// transaction", which is the same class of lie the write-ahead ordering
	// exists to prevent (ADR-0074 Amendment 5 decision 5).
	//
	// collapsed_at is 0 for a progression that is still intact. Non-zero
	// means "the terminal you are reading is a tombstone; the transitions
	// that led to it were pruned at this time" — so a reader can tell a
	// short progression from a collapsed one rather than guessing.
	{
		Version: 10,
		SQLite: []string{
			`ALTER TABLE tx_outcomes ADD COLUMN collapsed_at BIGINT NOT NULL DEFAULT 0`,
		},
		Postgres: []string{
			`ALTER TABLE tx_outcomes ADD COLUMN collapsed_at BIGINT NOT NULL DEFAULT 0`,
		},
	},
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
