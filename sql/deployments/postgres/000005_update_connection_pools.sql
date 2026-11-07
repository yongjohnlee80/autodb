-- 000005 — connection pools: the budget a group of connections shares.
--
-- db_pool_connections  one row per pool. max_conns is how many sockets autodb
--                      may hold, in total, to every connection in the pool:
--                      one control lane, a headroom share for exec, TUI and
--                      internal work, and the rest for front-door sessions.
--                      Pool 1, "default", is seeded at 25 and can't be deleted.
-- connections.pool_id  the pool a connection belongs to; every existing row
--                      joins "default". MySQL and SQLite connections are
--                      outside every pool whatever this column says.
--
-- max_conns' floor (3) is core/exec's minPoolBudget and its ceiling (10000) is
-- meta.MaxPoolConns; a test ties each to its constant.
--
-- Additive, with defaults: a binary before this script still reads and writes
-- every table it knows. IF NOT EXISTS and ON CONFLICT throughout.

CREATE TABLE IF NOT EXISTS db_pool_connections (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    max_conns BIGINT NOT NULL DEFAULT 25 CHECK (max_conns BETWEEN 3 AND 10000),
    created_by BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL);
INSERT INTO db_pool_connections (id, name, max_conns, created_at, updated_at)
    VALUES (1, 'default', 25, EXTRACT(EPOCH FROM now())::bigint, EXTRACT(EPOCH FROM now())::bigint)
    ON CONFLICT (id) DO NOTHING;
SELECT setval(pg_get_serial_sequence('db_pool_connections', 'id'), GREATEST((SELECT MAX(id) FROM db_pool_connections), 1));
ALTER TABLE connections ADD COLUMN IF NOT EXISTS pool_id BIGINT NOT NULL DEFAULT 1 REFERENCES db_pool_connections(id);
