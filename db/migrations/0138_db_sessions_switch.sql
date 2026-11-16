-- Database sessions as a whole, on or off by an admin (design/DATABASE-SESSIONS.md §5; #2835): off by default, so a
-- Canyon that upgrades opens no new access path into app databases until an admin turns them on. Turning them off
-- closes every open database session turned_off.
ALTER TABLE org_preferences ADD COLUMN db_sessions text NOT NULL DEFAULT 'off' CHECK (db_sessions IN ('off', 'on'));
ALTER TABLE db_sessions DROP CONSTRAINT db_sessions_close_reason_check,
    ADD CONSTRAINT db_sessions_close_reason_check CHECK (close_reason IN ('signed_out', 'access_changed', 'setting_changed', 'database_gone', 'expired', 'held', 'idle', 'turned_off', 'closed', 'ended', 'replaced'));
