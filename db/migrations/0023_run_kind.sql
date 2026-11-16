-- A run reviews a package (gate, and a deploy is a gate run that goes on to build and run) or promotes
-- the image preview runs to prod (promote). The receipt, the fleet and the gate-runs list read gate
-- runs only; a promote run is read by its id, its timeline the promotion's progress.
ALTER TABLE runs ADD COLUMN kind text NOT NULL DEFAULT 'gate' CHECK (kind IN ('gate', 'promote'));
