-- Reverts 000005_update_connection_pools.sql: every connection leaves its
-- pool, and the pools go.

ALTER TABLE connections DROP COLUMN pool_id;
DROP TABLE IF EXISTS db_pool_connections;
