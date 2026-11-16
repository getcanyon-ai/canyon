-- The patch run to preview (design/PATCHING.md §4.3; contracts/routes.md, Patch runs): its new failure
-- reasons, with not_platform_only beside the one of that name, and its author.
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start', 'no_room', 'scan_no_room', 'image_gone',
    'not_platform_only', 'patch_regressed', 'patch_ineffective', 'reverted'));
ALTER TABLE runs ADD COLUMN not_platform_only text CHECK (not_platform_only IN ('layers_changed', 'config_changed')),
    ADD CONSTRAINT runs_not_platform_only CHECK ((not_platform_only IS NOT NULL) = (failure_reason IS NOT DISTINCT FROM 'not_platform_only'));

-- A patch run's author: the author of the run whose image it rebases — preview's row's gate_run, that run's
-- own author when it was a patch run — as that run recorded it, its started_by and started_by_github. NULL
-- on every other run, whose author is who started it, and on a patch run whose image's run recorded none:
-- then no approval covers its blocking findings.
ALTER TABLE runs ADD COLUMN author_kind text CHECK (author_kind IN ('person', 'agent')),
    ADD COLUMN author_subject text,
    ADD COLUMN author_display text,
    ADD COLUMN author_github  bigint,
    ADD CONSTRAINT runs_author CHECK ((author_kind IS NULL) = (author_subject IS NULL) AND (author_kind IS NULL) = (author_display IS NULL)
        AND (author_kind IS NULL OR kind = 'patch') AND (author_github IS NULL OR author_kind IS NOT NULL));
