-- Reverts 000005_update_connection_pools.sql: every connection leaves its
-- pool, and the pools go. The column goes first, because of its foreign key.

ALTER TABLE connections DROP COLUMN IF EXISTS pool_id;
DROP TABLE IF EXISTS db_pool_connections;
