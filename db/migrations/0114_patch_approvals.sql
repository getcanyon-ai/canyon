-- Production, asked (design/PATCHING.md §4.5; contracts/routes.md, Production approvals): the org's patching,
-- off or ask, and a patch approval, a production approval of kind patch that Canyon files for a patch whose watch
-- passed in preview. Its gate_run is the patch run; it records the platform identity, the image production ran
-- when it was filed (replaces_image), and what the patch fixed, names and versions only. Expand only: every
-- column defaulted or nullable, so the previous binary reads and writes its rows as before.
ALTER TABLE org_preferences ADD COLUMN patching text NOT NULL DEFAULT 'ask' CHECK (patching IN ('off', 'ask'));

ALTER TABLE promotion_approvals
    ADD COLUMN kind           text NOT NULL DEFAULT 'promotion' CHECK (kind IN ('promotion', 'patch')),
    ADD COLUMN patch_identity text CHECK (patch_identity ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN replaces_image text,
    ADD COLUMN fixes          jsonb,
    ADD CONSTRAINT promotion_approvals_patch_check CHECK ((kind = 'patch') = (patch_identity IS NOT NULL)
        AND (kind = 'patch') = (replaces_image IS NOT NULL) AND (kind = 'patch') = (fixes IS NOT NULL));

-- A team's denial stands for that app and identity: the tick reads it before it files again.
CREATE INDEX promotion_approvals_patch_denied ON promotion_approvals (project_id, patch_identity) WHERE kind = 'patch' AND state = 'denied';
