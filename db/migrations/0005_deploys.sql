-- A failed build is the user's fix, not Canyon's: failure_reason build (contracts/events.md).
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build'));

-- What is deployed where: one row per project and environment, upserted; the history is the
-- ledger's deploy.completed rows. image is the full reference (canyon/<app>@sha256:…): promote
-- ships exactly this (executor invariant 4); the API serves the digest half.
CREATE TABLE deploys (
    project_id  text NOT NULL REFERENCES projects(id),
    env         text NOT NULL CHECK (env IN ('preview', 'prod')),
    package_id  text NOT NULL,
    image       text NOT NULL,
    url         text NOT NULL,
    gate_run    text NOT NULL REFERENCES runs(id),
    deployed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, env),
    FOREIGN KEY (package_id, project_id) REFERENCES packages (id, project_id)
);
