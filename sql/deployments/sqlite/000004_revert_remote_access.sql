-- Reverts 000004_update_remote_access.sql: the remote-access tables and the
-- three session columns go.
--
-- Every registered SSH key, enrolled device, device address, block and denial
-- claim is dropped with its table. A session keeps its row; a remote one loses
-- only its binding, and a binary without this script never issued one.

DROP INDEX IF EXISTS idx_sessions_device;
ALTER TABLE sessions DROP COLUMN detached_until;
ALTER TABLE sessions DROP COLUMN attached_conn;
ALTER TABLE sessions DROP COLUMN device_id;
DROP TABLE IF EXISTS remote_denial_events;
DROP TABLE IF EXISTS remote_ip_blocks;
DROP TABLE IF EXISTS remote_device_ips;
DROP TABLE IF EXISTS remote_devices;
DROP TABLE IF EXISTS user_ssh_keys;
