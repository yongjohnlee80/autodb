-- 000005 — connection pools: the budget a group of connections shares.
-- See postgres/000005_update_connection_pools.sql for what each part is.
--
-- connections.pool_id has no REFERENCES here: SQLite refuses ADD COLUMN with a
-- foreign key and a non-NULL default. Pool admin checks membership itself.
-- SQLite has no ADD COLUMN IF NOT EXISTS; the ledger runs every script once.

CREATE TABLE IF NOT EXISTS db_pool_connections (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    max_conns INTEGER NOT NULL DEFAULT 25 CHECK (max_conns BETWEEN 3 AND 10000),
    created_by INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL);
INSERT OR IGNORE INTO db_pool_connections (id, name, max_conns, created_at, updated_at)
    VALUES (1, 'default', 25, CAST(strftime('%s', 'now') AS INTEGER), CAST(strftime('%s', 'now') AS INTEGER));
ALTER TABLE connections ADD COLUMN pool_id INTEGER NOT NULL DEFAULT 1;
