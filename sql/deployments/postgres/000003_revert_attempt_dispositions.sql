-- Reverts 000003_update_attempt_dispositions.sql: the indexes and the columns go.
--
-- A row's status is kept, and it keeps the old vocabulary except 'unknown',
-- which an older binary shows as the word it is. A refusal still reads
-- 'error', as it always did; the disposition that told it apart is dropped.

DROP INDEX IF EXISTS idx_audit_conn_created;
DROP INDEX IF EXISTS idx_history_conn_started;
DROP INDEX IF EXISTS idx_history_attempt;
ALTER TABLE audit_log DROP COLUMN IF EXISTS conn_id;
ALTER TABLE audit_log DROP COLUMN IF EXISTS attempt_id;
ALTER TABLE script_history DROP COLUMN IF EXISTS disposition;
ALTER TABLE script_history DROP COLUMN IF EXISTS attempt_owner;
ALTER TABLE script_history DROP COLUMN IF EXISTS attempt_id;
