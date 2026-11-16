-- Sessions, the audit trail, project owners (design/ARCHITECTURE.md: every stored actor is the
-- typed triple {kind, subject, display}), and the project columns the routes serve.

CREATE TABLE sessions (
    id            text PRIMARY KEY,                -- the cookie value: 32 random bytes, hex
    actor_kind    text NOT NULL CHECK (actor_kind IN ('person', 'agent', 'canyon')),
    actor_subject text NOT NULL,
    actor_display text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- The trail outlives everything it names (a deleted project, a run retention removed) and the
-- stage writes it before the handler runs: no foreign keys, on purpose.
CREATE TABLE audit_events (
    id            text PRIMARY KEY,                -- ev_<hex>
    ts            timestamptz NOT NULL DEFAULT now(),
    actor_kind    text NOT NULL CHECK (actor_kind IN ('person', 'agent', 'canyon')),
    actor_subject text NOT NULL,
    actor_display text NOT NULL DEFAULT '',
    action        text NOT NULL,                   -- the route pattern: "POST /api/v1/projects/{id}/owner"
    project_id    text,
    run_id        text,
    detail        jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_events_ts ON audit_events (ts);

CREATE TABLE project_owners (
    project_id text NOT NULL REFERENCES projects(id),
    kind       text NOT NULL CHECK (kind IN ('person', 'agent', 'canyon')),
    subject    text NOT NULL,
    display    text NOT NULL DEFAULT '',
    set_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, kind, subject)
);

ALTER TABLE projects
    ADD COLUMN name text NOT NULL DEFAULT '',
    ADD COLUMN stack text NOT NULL DEFAULT '',
    ADD COLUMN deleted_at timestamptz,
    DROP COLUMN owner,                            -- owners are the table above; nothing read this
    -- the id names a directory on disk (runs.ValidProject is the same pattern): every writer passes here
    ADD CONSTRAINT projects_id_shape CHECK (id ~ '^[a-z][a-z0-9-]*$');
UPDATE projects SET name = id WHERE name = '';                -- rows from before this migration: the name is the id (the routes always set it)
