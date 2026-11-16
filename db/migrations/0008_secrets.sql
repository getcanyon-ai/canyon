-- A secret is one row per (project, env, name) — contracts/routes.md, the secrets rows. The
-- value is sealed under its own data key and the data key under the key file, each bound to
-- the row (vault/). A PUT overwrites the row and bumps version: no history, the audit trail
-- answers who and when. scope is the contract's `where`: the build receives the build values,
-- the container the runtime values.
CREATE TABLE secrets (
    project_id     text NOT NULL REFERENCES projects(id),
    env            text NOT NULL CHECK (env IN ('preview', 'prod')),
    name           text NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_]{0,127}$' AND name <> 'PORT'),
    scope          text NOT NULL CHECK (scope IN ('runtime', 'build')),
    version        integer NOT NULL DEFAULT 1,
    sealed_key     bytea NOT NULL,
    sealed_value   bytea NOT NULL,
    set_by_kind    text NOT NULL CHECK (set_by_kind IN ('person', 'agent', 'canyon')),
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL DEFAULT '',
    set_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, env, name)
);
