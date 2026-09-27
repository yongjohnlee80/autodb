-- 000002 — a connection can be ARCHIVED rather than deleted.
--
-- A connection with recorded history cannot be deleted: the history keeps a
-- foreign key to it, by design, so the record outlives the connection. An
-- archived connection is what deleting one becomes instead — its stored DSN
-- wiped for good, its grants, workspace links and front-door exposure removed,
-- the access tokens bound to it revoked, and its name freed by a rename — and
-- it keeps its row, and its id, for the history that names it.
--
-- archived_at is the unix second it was archived; 0 is a live connection,
-- which is every existing row. Additive, with a default: a binary before this
-- script still reads and writes the store. IF NOT EXISTS, as the engine allows.

ALTER TABLE connections ADD COLUMN IF NOT EXISTS archived_at BIGINT NOT NULL DEFAULT 0;
