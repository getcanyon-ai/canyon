-- The control plane's first tables. Timestamp-named migrations are never edited after merge
-- (AGENTS.md); later shape changes are new files.

CREATE TABLE projects (
    id         text PRIMARY KEY,                 -- the app name, e.g. claims-portal
    owner      text NOT NULL DEFAULT '',
    harness    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A package is what an upload became (contracts/routes.md): the tree, the commit holding it,
-- and where it came from.
CREATE TABLE packages (
    id           text PRIMARY KEY,               -- pkg_<hash>
    project_id   text NOT NULL REFERENCES projects(id),
    tree_hash    text NOT NULL,
    commit       text NOT NULL,
    size_bytes   bigint NOT NULL,
    source_kind  text NOT NULL CHECK (source_kind IN ('upload', 'repo')),
    source_ref   text NOT NULL DEFAULT '',
    source_actor text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, project_id)                      -- lets runs tie a package to its project
);

CREATE TABLE runs (
    id             text PRIMARY KEY,             -- gr_<id>
    project_id     text NOT NULL REFERENCES projects(id),
    package_id     text NOT NULL,
    state          text NOT NULL CHECK (state IN ('pending', 'running', 'clean', 'blocked', 'failed')),
    failure_reason text CHECK (failure_reason IN ('interrupted', 'error', 'model')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz,
    UNIQUE (id, project_id),                     -- lets timeline rows tie a run to its project
    CHECK ((state = 'failed') OR failure_reason IS NULL),
    FOREIGN KEY (package_id, project_id) REFERENCES packages (id, project_id)  -- a run reviews its own project's package
);
-- One run at a time per project (durable runs: a queued run says so, it never races).
CREATE UNIQUE INDEX runs_one_active_per_project ON runs (project_id) WHERE state IN ('pending', 'running');

-- One row of the run or org timeline (contracts/events.md). id is the paging cursor within a
-- scope; rows are append-only.
CREATE TABLE timeline_rows (
    id         bigserial PRIMARY KEY,
    ts         timestamptz NOT NULL DEFAULT now(),
    scope      text NOT NULL CHECK (scope IN ('run', 'org')),
    run_id     text REFERENCES runs(id),
    project_id text NOT NULL REFERENCES projects(id),
    kind       text NOT NULL,
    data       jsonb NOT NULL,
    CHECK ((scope = 'run') = (run_id IS NOT NULL)),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id)  -- a run row is its project's
);
CREATE INDEX timeline_rows_run ON timeline_rows (run_id, id) WHERE scope = 'run';
CREATE INDEX timeline_rows_org ON timeline_rows (id) WHERE scope = 'org';

-- The timeline is the ledger: rows are never edited or deleted (contracts/events.md).
CREATE FUNCTION timeline_rows_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'timeline_rows is append-only';
END $$;
CREATE TRIGGER timeline_rows_append_only BEFORE UPDATE OR DELETE ON timeline_rows
    FOR EACH ROW EXECUTE FUNCTION timeline_rows_append_only();
CREATE TRIGGER timeline_rows_no_truncate BEFORE TRUNCATE ON timeline_rows
    FOR EACH STATEMENT EXECUTE FUNCTION timeline_rows_append_only();
