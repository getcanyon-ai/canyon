-- The fleet read (contracts/routes.md row 75). deployed_by is recorded on the deploy row from the run's actor (row 47): older rows answer null, no backfill. The indexes serve the derivation's per-project lookups.
ALTER TABLE deploys
    ADD COLUMN deployed_by_kind    text CHECK (deployed_by_kind IN ('person', 'agent', 'canyon')),
    ADD COLUMN deployed_by_subject text,
    ADD COLUMN deployed_by_display text;
CREATE INDEX runs_latest       ON runs     (project_id, created_at DESC, id DESC);
CREATE INDEX packages_latest   ON packages (project_id, created_at DESC, id DESC);
CREATE INDEX timeline_verdicts ON timeline_rows (project_id, id DESC) WHERE kind = 'verdict';
