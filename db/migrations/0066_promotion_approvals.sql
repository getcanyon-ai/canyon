-- Production is approved in the dashboard (design/AUTH.md §4.5, #1820): the org's switch, off by default, and
-- the approvals the CLI files while it is on (contracts/routes.md, Production approvals). An approval names the
-- run asked for and the package and image preview ran then, so a person approves exactly the version shown.
-- Its state is pending until decided; expired and outdated are read, never stored.
ALTER TABLE org_preferences ADD COLUMN prod_approval boolean NOT NULL DEFAULT false;

CREATE TABLE promotion_approvals (
    code                 text PRIMARY KEY CHECK (code ~ '^[0-9A-HJKMNP-TV-Z]{8}$'), -- Crockford base32, shown XXXX-XXXX
    project_id           text NOT NULL REFERENCES projects(id),
    gate_run             text NOT NULL,
    package_id           text NOT NULL,
    image                text NOT NULL,
    requested_by_kind    text NOT NULL CHECK (requested_by_kind IN ('person', 'agent', 'canyon')),
    requested_by_subject text NOT NULL,
    requested_by_display text NOT NULL,
    requested_via        text NOT NULL,
    requested_at         timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    state                text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'denied')),
    decided_by_kind      text CHECK (decided_by_kind IN ('person', 'agent', 'canyon')),
    decided_by_subject   text,
    decided_by_display   text,
    decided_at           timestamptz,
    run_id               text
);

-- At most one pending approval per app: the next request deletes the one before it.
CREATE UNIQUE INDEX promotion_approvals_one_pending ON promotion_approvals (project_id) WHERE state = 'pending';
