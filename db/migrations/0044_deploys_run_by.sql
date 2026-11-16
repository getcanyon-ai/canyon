-- Who runs the environment (design/HAND-OFF.md §1, "each deploy row records the run_by it was
-- made under"): canyon, a deploy the pipeline ran itself, or org, one the org's platform reported
-- (POST /releases/:id/report). Recorded when the row is written, on the insert and on the upsert
-- alike; a row written before this column answers canyon (contracts/routes.md, the project read).
ALTER TABLE deploys ADD COLUMN run_by text NOT NULL DEFAULT 'canyon' CHECK (run_by IN ('canyon', 'org'));
