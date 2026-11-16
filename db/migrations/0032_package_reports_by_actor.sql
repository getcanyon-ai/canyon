-- The per-actor rate limit counts the actor's attaches of the last hour (runs.ActorLimited), inside every
-- gate-run start under the ledger lock and before every attach: read by actor and time, never a scan of a
-- table that only grows (0031: every attach is a row, never deleted).
CREATE INDEX package_reports_by_actor ON package_reports (attached_by_kind, attached_by_subject, attached_at);
