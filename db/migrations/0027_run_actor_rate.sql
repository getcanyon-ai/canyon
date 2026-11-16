-- The per-actor gate-run limit (contracts/routes.md, POST /api/v1/projects/:id/gate-runs) counts an actor's runs of
-- the last hour; runs_latest (0009) already serves the per-project count.
CREATE INDEX runs_by_actor ON runs (started_by_kind, started_by_subject, created_at);
