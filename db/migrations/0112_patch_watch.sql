-- The patch watch and its revert (design/PATCHING.md §4.4; contracts/routes.md, Patch runs): what preview's
-- row was before the patch replaced it, as the patch recorded it, which a revert puts back — its image, gate
-- run and fingerprint among them, names and digests, never a value — and the run's own mark that it is
-- reverting, set by compare-and-set so a person's request and a revert never both /run. Neither is a run state.
ALTER TABLE runs ADD COLUMN patch_replaced jsonb CHECK (patch_replaced IS NULL OR kind = 'patch'),
    ADD COLUMN reverting_at timestamptz CHECK (reverting_at IS NULL OR kind = 'patch');
