-- When a session last made a request, touched at most hourly (#2953): the usage report's people and agents active in
-- the last 1, 7 and 28 days count a person who only reads, which the sign-in alone (created_at) would miss. NULL
-- until the session's first request after this migration.
ALTER TABLE sessions ADD COLUMN last_seen_at timestamptz;
