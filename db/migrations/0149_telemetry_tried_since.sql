-- Until a first report reaches the collector, the server tries again for 90 days (contracts/routes.md, Telemetry, Sending):
-- tried_since is the first of those tries, kept with claimed_at, the last, so a restart reads the wait back. Null once a
-- report reaches it. A row off as 'unreachable' before this stays off.
ALTER TABLE telemetry ADD COLUMN tried_since timestamptz CHECK (tried_since IS NULL OR claimed_at IS NOT NULL);
