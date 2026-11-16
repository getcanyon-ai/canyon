-- A resource an app declared, one row per (project, env, name): which driver serves it and where
-- it stands. It holds no outputs: a ready answer's outputs are sealed as ordinary secret rows
-- (origin 'provisioned'), so the run path reads them as it reads any secret. The row is removed
-- only when its driver answers gone, never on a failed delete: dropping it would orphan the
-- real database. 'deleting' marks a resource whose project is being torn down; Ensure refuses it.
CREATE TABLE resources (
    project_id text NOT NULL REFERENCES projects(id),
    env        text NOT NULL CHECK (env IN ('preview', 'prod')),
    name       text NOT NULL,
    type       text NOT NULL,
    driver     text NOT NULL,
    state      text NOT NULL CHECK (state IN ('pending', 'ready', 'failed', 'deleting')),
    message    text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, env, name)
);
-- Who wrote a secret row: a person or agent through the secrets routes ('user'), or Canyon from
-- a driver's ready answer ('provisioned'). NULL reads as 'user', every row before this column.
-- Expand only: nullable, no default, no data movement (as 0017's key_ref).
ALTER TABLE secrets ADD COLUMN origin text CHECK (origin IN ('user', 'provisioned'));
