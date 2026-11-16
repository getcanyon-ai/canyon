-- A release: one instruction to the org's platform under run_by: org (design/HAND-OFF.md §3;
-- release.schema.json's deploy/remove; contracts/routes.md, POST /releases/:id/report and
-- GET /releases/:id/access-token). delivery is the same across every retry of it, re-stamped and
-- re-signed; state starts sent, moves to accepted on a 2xx, and to undelivered or refused when
-- Canyon gives up. deployed/failed/removed are the report route's alone (PR 4).
CREATE TABLE releases (
    id         text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    env        text NOT NULL CHECK (env IN ('preview', 'prod')),
    run_id     text NOT NULL,
    package_id text NOT NULL,
    event      text NOT NULL CHECK (event IN ('deploy', 'remove')),
    image      text NOT NULL,
    delivery   uuid NOT NULL,
    state      text NOT NULL CHECK (state IN ('sent', 'accepted', 'deployed', 'failed', 'removed', 'undelivered', 'refused')),
    sent_at    timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
