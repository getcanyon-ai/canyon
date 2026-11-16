-- The promote receipt (contracts/routes.md "The promote receipt"). An approval covers one finding of one
-- project across runs and uploads until it expires; the receipt reads them by project_id, the key's prefix.
-- promoted_by is the actor who clicked promote, recorded on the prod row by the promote route: older rows
-- answer null, no backfill (the deployed_by precedent).
CREATE TABLE approvals (
    project_id          text NOT NULL REFERENCES projects(id),
    rule                text NOT NULL,
    fingerprint         text NOT NULL,
    reason              text NOT NULL,
    expires_at          timestamptz NOT NULL,
    approved_by_kind    text NOT NULL CHECK (approved_by_kind IN ('person', 'agent', 'canyon')),
    approved_by_subject text NOT NULL,
    approved_by_display text NOT NULL DEFAULT '',
    approved_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, rule, fingerprint)
);
ALTER TABLE deploys
    ADD COLUMN promoted_by_kind    text CHECK (promoted_by_kind IN ('person', 'agent', 'canyon')),
    ADD COLUMN promoted_by_subject text,
    ADD COLUMN promoted_by_display text;
