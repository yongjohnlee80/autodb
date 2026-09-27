-- 000003 — every attempted statement gets an identity and one disposition,
-- and the audit log records the connection an entry is about.
--
-- script_history:
--   attempt_id    the attempt's identity, minted once per dispatch decision.
--                 '' on every row written before this script.
--   attempt_owner the epoch of the daemon that made the attempt; a row whose
--                 owner is not the serving daemon's belongs to a process that
--                 is gone (the instance lease is exclusive), and a row still
--                 'running' is settled by that daemon.
--   disposition   what the attempt did — completed, failed, refused,
--                 rolled_back, unknown — written ONCE, never changed. status
--                 keeps answering what became of the effect.
-- audit_log:
--   attempt_id    the same identity, on every row about an attempt.
--   conn_id       the connection an entry is about; 0 for rows before this
--                 script and for entries about no connection. No foreign key:
--                 an audit row outlives anything it names.
--
-- Additive, with defaults: a binary before this script still reads and writes
-- both tables. The indexes use IF NOT EXISTS; SQLite has no ADD COLUMN IF NOT EXISTS, and the
-- ledger runs every script once.

ALTER TABLE script_history ADD COLUMN attempt_id TEXT NOT NULL DEFAULT '';
ALTER TABLE script_history ADD COLUMN attempt_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE script_history ADD COLUMN disposition TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN attempt_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN conn_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_history_attempt ON script_history(attempt_id);
CREATE INDEX IF NOT EXISTS idx_history_conn_started ON script_history(connection_id, started_at);
CREATE INDEX IF NOT EXISTS idx_audit_conn_created ON audit_log(conn_id, created_at);
