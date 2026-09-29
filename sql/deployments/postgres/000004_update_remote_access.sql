-- 000004 — remote access: SSH keys on a user's
-- profile, the device each key enrolled, the addresses a device has used, the
-- per-address failure blocks and the denial claims their replay is keyed by,
-- and the three columns that bind a session to a device and a connection.
--
-- user_ssh_keys       a user's registered SSH public keys. A fingerprint
--                     names one LIVE key (revoked_at = 0) across all users;
--                     a revoked row stays as history.
-- remote_devices      the device an SSH key enrolled: one live device per key.
-- remote_device_ips   the addresses a device has connected from, so a new one
--                     is noticed ("new IP for a device").
-- remote_ip_blocks    one row per source prefix (IPv4 /32, IPv6 /64): its
--                     consecutive failures and the block they set.
-- remote_denial_events the idempotency claim for a denial: its event_id is
--                     the key a replay after a crash is recognised by.
-- sessions            device_id: the device a remote token is bound to, 0 for
--                     a local session. attached_conn: the connection that owns
--                     a remote session, '' when none. detached_until: the end
--                     of a dropped session's reconnect grace, 0 when attached.
--
-- "None" is 0 or '' with NOT NULL, as the rest of this schema writes it. User
-- ids an audit trail names (added_by, revoked_by, unblocked_by) carry no
-- foreign key: a record outlives what it names.
--
-- Additive, with defaults: a binary before this script still reads and writes
-- every table it knows. IF NOT EXISTS throughout; SQLite has no ADD COLUMN IF
-- NOT EXISTS, and the ledger runs every script once.

CREATE TABLE IF NOT EXISTS user_ssh_keys (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label TEXT NOT NULL DEFAULT '',
    public_key TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    added_by BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL DEFAULT 0,
    revoked_at BIGINT NOT NULL DEFAULT 0,
    revoked_by BIGINT NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ssh_keys_live_fingerprint ON user_ssh_keys(fingerprint) WHERE revoked_at = 0;
CREATE INDEX IF NOT EXISTS idx_ssh_keys_user ON user_ssh_keys(user_id);

CREATE TABLE IF NOT EXISTS remote_devices (
    id BIGSERIAL PRIMARY KEY,
    ssh_key_id BIGINT NOT NULL REFERENCES user_ssh_keys(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_public_key TEXT NOT NULL,
    device_fingerprint TEXT NOT NULL,
    enrolled_at BIGINT NOT NULL,
    enrolled_ip TEXT NOT NULL,
    key_created_at BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL DEFAULT 0,
    revoked_at BIGINT NOT NULL DEFAULT 0,
    revoked_by BIGINT NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_devices_live_key ON remote_devices(ssh_key_id) WHERE revoked_at = 0;
CREATE INDEX IF NOT EXISTS idx_remote_devices_user ON remote_devices(user_id);

CREATE TABLE IF NOT EXISTS remote_device_ips (
    id BIGSERIAL PRIMARY KEY,
    device_id BIGINT NOT NULL REFERENCES remote_devices(id) ON DELETE CASCADE,
    ip TEXT NOT NULL,
    first_seen_at BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL,
    UNIQUE (device_id, ip));

CREATE TABLE IF NOT EXISTS remote_ip_blocks (
    prefix TEXT PRIMARY KEY,
    consecutive_failures BIGINT NOT NULL DEFAULT 0,
    last_failure_at BIGINT NOT NULL DEFAULT 0,
    blocked_until BIGINT NOT NULL DEFAULT 0,
    unblocked_by BIGINT NOT NULL DEFAULT 0,
    unblocked_at BIGINT NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS remote_denial_events (
    event_id TEXT PRIMARY KEY,
    prefix TEXT NOT NULL,
    occurred_at BIGINT NOT NULL,
    reason TEXT NOT NULL,
    offered_key_fp TEXT NOT NULL DEFAULT '',
    user_id BIGINT NOT NULL DEFAULT 0,
    audit_id BIGINT NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_denials_prefix_time ON remote_denial_events(prefix, occurred_at);

ALTER TABLE sessions ADD COLUMN device_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN attached_conn TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN detached_until BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_sessions_device ON sessions(device_id);
