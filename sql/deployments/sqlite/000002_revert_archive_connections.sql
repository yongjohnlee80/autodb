-- Reverts 000002_update_archive_connections.sql: the column goes.
--
-- An archived connection is NOT revived by this: its DSN was wiped when it was
-- archived and stays wiped, and it keeps the name "<name> (archived <id>)". A
-- binary before 000002 lists it among the live connections, and opening it
-- fails — there is no DSN to open.

ALTER TABLE connections DROP COLUMN archived_at;
