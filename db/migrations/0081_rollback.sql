-- Rollback (design/OPERATIONS.md §1; contracts/routes.md, the gate-runs row's rollback_to): production's
-- history, one row per promotion that went live there, appended by the shared production writer alone
-- (runs.WriteProdTx) in the deploy row's transaction. replaces is the image production ran until then, so
-- the server keeps it on the next /run; kept turns false once a /run leaves the image out of its keep, or a
-- promotion's /run finds it gone, and never back; under the org no /run cleans its registry, so its images
-- stay kept (design/OPERATIONS.md §1.6). patched marks an image Canyon's patch produced; egress is the spec production ran
-- it under, which a rollback to it carries (design/EGRESS.md §2.5).
CREATE TABLE deploy_history (
    id                  bigserial PRIMARY KEY,
    project_id          text NOT NULL REFERENCES projects(id),
    package_id          text NOT NULL,
    image               text NOT NULL,
    replaces            text,
    gate_run            text NOT NULL,
    run                 text NOT NULL,
    deployed_at         timestamptz NOT NULL DEFAULT now(),
    promoted_by_kind    text CHECK (promoted_by_kind IN ('person', 'agent', 'canyon')),
    promoted_by_subject text,
    promoted_by_display text,
    patched             boolean NOT NULL DEFAULT false,
    kept                boolean NOT NULL DEFAULT true,
    egress              jsonb,
    FOREIGN KEY (package_id, project_id) REFERENCES packages (id, project_id)
);

-- The status read's envs.prod.earlier[] and the keep's newest row read one app's history newest first.
CREATE INDEX deploy_history_newest ON deploy_history (project_id, deployed_at DESC);

-- What production runs now is its history's first row, so the next promotion records what it replaced and
-- keeps it.
INSERT INTO deploy_history (project_id, package_id, image, gate_run, run, deployed_at, promoted_by_kind, promoted_by_subject, promoted_by_display, egress)
    SELECT project_id, package_id, image, gate_run, gate_run, deployed_at, promoted_by_kind, promoted_by_subject, promoted_by_display, egress
    FROM deploys WHERE env = 'prod';

-- A rollback review: the digest it was asked to put back, the person's reason, the image production ran
-- as it started — a promotion of it, or an approval of one, is outdated once production runs another —
-- and the image it records to ship once its review passes. Rollback reviews count in a limit of their own.
ALTER TABLE runs
    ADD COLUMN rollback_to       text,
    ADD COLUMN rollback_reason   text,
    ADD COLUMN rollback_replaces text,
    ADD COLUMN ships             text;

-- A rollback review whose image is no longer kept ends failed/image_gone: it never builds.
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start', 'no_room', 'scan_no_room', 'image_gone'));
