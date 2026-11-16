-- An approved patch's promotion watches production as the patch run watched preview (design/PATCHING.md §4.5;
-- contracts/routes.md, Production approvals and Patch runs): the promote run a patch approval started keeps
-- production's row as it found it, which its revert puts back, its own reverting mark and its watch_until. Widening
-- the checks keeps every row the previous binary writes. 0112 and 0113 left the checks unnamed, and one naming two
-- columns takes a generic name, so each is found by the column it constrains and dropped, then added again by name.
DO $$
DECLARE
    c record;
BEGIN
    FOR c IN SELECT DISTINCT con.conname FROM pg_constraint con JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
        WHERE con.conrelid = 'runs'::regclass AND con.contype = 'c' AND a.attname IN ('patch_replaced', 'reverting_at', 'watch_until')
    LOOP
        EXECUTE format('ALTER TABLE runs DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;

-- A promote run ships a patch when a patch approval names it as its run: read by every start of the app's runs.
CREATE INDEX promotion_approvals_patch_run ON promotion_approvals (run_id) WHERE kind = 'patch';

ALTER TABLE runs ADD CONSTRAINT runs_patch_replaced_kind CHECK (patch_replaced IS NULL OR kind IN ('patch', 'promote')),
    ADD CONSTRAINT runs_reverting_at_kind CHECK (reverting_at IS NULL OR kind IN ('patch', 'promote')),
    ADD CONSTRAINT runs_watch_until_kind CHECK (watch_until IS NULL OR kind IN ('patch', 'promote'));
