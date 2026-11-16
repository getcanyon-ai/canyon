-- Telemetry is on by default (the owner, 2026-11-12, as Grafana and Coder do): the first report goes at the first day's
-- send, with no admin having to see it first, so 0131's seen_at is read by nothing and goes.
ALTER TABLE telemetry DROP COLUMN seen_at;
