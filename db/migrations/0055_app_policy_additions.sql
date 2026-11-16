-- An app's additions (contracts/routes.md, PUT and DELETE /api/v1/projects/:id/policies/:rule;
-- design/EXPERIENCE.md D14): the app's own state for one rule in each gate it names, NULL for a gate it
-- does not, each stricter than the org's when it was set. A PUT replaces the row whole and a DELETE
-- removes it; the audit trail keeps their history. The project's runs use the org's snapshot with these
-- applied, in each gate the stricter of the two.
CREATE TABLE app_policy_additions (
    project_id     text NOT NULL REFERENCES projects(id),
    rule           text NOT NULL,
    state          text CHECK (state IN ('blocking', 'advisory', 'off')),
    production     text CHECK (production IN ('blocking', 'advisory', 'off')),
    set_by_kind    text NOT NULL CHECK (set_by_kind IN ('person', 'agent', 'canyon')),
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL DEFAULT '',
    set_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, rule),
    CHECK (state IS NOT NULL OR production IS NOT NULL)
);
