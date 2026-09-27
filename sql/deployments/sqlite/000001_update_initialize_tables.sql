-- 000001 — the meta store's schema at v17, the last version of the in-code
-- migration list (docs/ops/schema-scripts.md). The BASELINE: it has no revert.
--
-- SAFE ON A STORE THAT ALREADY HAS IT. Every statement is IF NOT EXISTS, so a
-- store at v17 runs this as a no-op and records it; a new store is created by
-- it. A store at v1..v16 is brought to v17 by the frozen legacy chain first,
-- in the same transaction — SQLite has no ADD COLUMN IF NOT EXISTS, so no
-- script could do that.
--
-- IT IS THE LEGACY CHAIN'S SCHEMA: columns in the order the legacy ALTERs
-- appended them, so a store 000001 created and one it adopted agree column
-- for column. A test compares the two catalogs and fails on any difference.

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
    pass_hash BLOB NOT NULL,
    disabled INTEGER NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    mk_wrapped BLOB NOT NULL DEFAULT x'',
    options TEXT NOT NULL DEFAULT '{}');

CREATE TABLE IF NOT EXISTS connections (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    engine TEXT NOT NULL CHECK (engine IN ('postgres','mysql','sqlite')),
    dsn_enc BLOB NOT NULL,
    created_by INTEGER NOT NULL REFERENCES users(id),
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'v1compat',
    debug INTEGER NOT NULL DEFAULT 0,
    pool_max_conns INTEGER NOT NULL DEFAULT 0,
    target_db TEXT NOT NULL DEFAULT '',
    frontdoor_exposed INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS workspaces (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS workspace_connections (
    id INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    UNIQUE (workspace_id, connection_id));

CREATE TABLE IF NOT EXISTS grants (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('admin','editor','reader')),
    granted_by INTEGER NOT NULL REFERENCES users(id),
    created_at BIGINT NOT NULL,
    UNIQUE (user_id, connection_id));

CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS script_history (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id),
    connection_id INTEGER NOT NULL REFERENCES connections(id),
    ip TEXT NOT NULL,
    script TEXT NOT NULL,
    started_at BIGINT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    row_count BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    tx_id TEXT NOT NULL DEFAULT '',
    suspended INTEGER NOT NULL DEFAULT 0);

-- audit_log has NO foreign keys by design: auditing must never fail, and
-- user_id 0 records pre-auth events.
CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL DEFAULT 0,
    ip TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    tx_id TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS ip_allowlist (
    id INTEGER PRIMARY KEY,
    cidr TEXT NOT NULL UNIQUE,
    note TEXT NOT NULL DEFAULT '',
    created_by INTEGER NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS store_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS tx_outcomes (
    id INTEGER PRIMARY KEY,
    tx_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    state TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    user_id INTEGER NOT NULL DEFAULT 0,
    connection_id INTEGER NOT NULL DEFAULT 0,
    history_id INTEGER NOT NULL DEFAULT 0,
    target_xid TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    collapsed_at BIGINT NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS tx_pending (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tx_id TEXT NOT NULL UNIQUE,
    connection_id BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    user_id BIGINT NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS user_ip_allowlist (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    cidr TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    UNIQUE (user_id, cidr));

CREATE TABLE IF NOT EXISTS pats (
    id INTEGER PRIMARY KEY,
    selector TEXT NOT NULL UNIQUE,
    secret_hash BLOB NOT NULL,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    allowed_ips TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL DEFAULT 0,
    revoked INTEGER NOT NULL DEFAULT 0,
    conn_id BIGINT NOT NULL DEFAULT 0,
    debug_cleartext INTEGER NOT NULL DEFAULT 0,
    UNIQUE (user_id, name));

CREATE TABLE IF NOT EXISTS keyslots (
    kind TEXT PRIMARY KEY CHECK (kind IN ('service')),
    wrapped BLOB NOT NULL,
    aad_version TEXT NOT NULL,
    created_by INTEGER NOT NULL REFERENCES users(id),
    created_at BIGINT NOT NULL);

CREATE INDEX IF NOT EXISTS idx_history_user ON script_history(user_id, started_at);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tx_outcomes_seq ON tx_outcomes(tx_id, seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tx_outcomes_terminal ON tx_outcomes(tx_id)
    WHERE state IN ('committed','rolled_back','outcome_unresolvable');
CREATE INDEX IF NOT EXISTS idx_tx_outcomes_state ON tx_outcomes(state, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_tx ON audit_log(tx_id);
CREATE INDEX IF NOT EXISTS idx_tx_pending_conn ON tx_pending(connection_id);
CREATE INDEX IF NOT EXISTS idx_tx_pending_order ON tx_pending(created_at, id);
CREATE INDEX IF NOT EXISTS idx_tx_pending_user ON tx_pending(user_id, created_at);
