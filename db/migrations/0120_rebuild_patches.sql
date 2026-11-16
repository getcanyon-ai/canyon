-- Rebuild patches (design/PATCHING.md §5; contracts/routes.md, Patch runs, A rebuild patch): the builder each build
-- asked GET /builder for, kept on the run that built and travelling with its image on the deploy rows; an identity's
-- kind and, for a rebuild's, its builder; the org's rebuilds switch; and the failure a rebuild's same_inputs proof
-- ends with, with which input differed. Nothing here is a secret: digests, kinds and names.
-- Expand only: every new column nullable or defaulted, so the previous binary reads and writes as before.
ALTER TABLE runs ADD COLUMN builder text,
    ADD COLUMN rebuilt_with text; -- a rebuild patch's: what it was rebuilt with, as its line same and its approval's proof name it
ALTER TABLE deploys ADD COLUMN builder text;

ALTER TABLE patch_identities ADD COLUMN kind text NOT NULL DEFAULT 'rebase' CHECK (kind IN ('rebase', 'rebuild')),
    ADD COLUMN builder text,
    ADD CONSTRAINT patch_identities_builder CHECK ((kind = 'rebuild') = (builder IS NOT NULL)),
    ADD CONSTRAINT patch_identities_rebuild_asked CHECK (kind = 'rebase' OR approved_at IS NULL);

ALTER TABLE org_preferences ADD COLUMN rebuilds text NOT NULL DEFAULT 'off' CHECK (rebuilds IN ('off', 'ask'));

-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start', 'no_room', 'scan_no_room', 'image_gone',
    'not_platform_only', 'not_same_inputs', 'patch_regressed', 'patch_ineffective', 'reverted'));
ALTER TABLE runs ADD COLUMN not_same_inputs text CHECK (not_same_inputs IN ('package', 'values', 'key_rotated', 'buildpacks', 'dependencies')),
    ADD CONSTRAINT runs_not_same_inputs CHECK ((not_same_inputs IS NOT NULL) = (failure_reason IS NOT DISTINCT FROM 'not_same_inputs'));
