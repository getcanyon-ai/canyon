-- A patch run's watch_until as its watch journaled it (contracts/routes.md, Patch runs, step 8), on its row, so
-- a check refused during the watch says until when (the gate-runs row's 409 watch, #2568). Not a secret: a time.
ALTER TABLE runs ADD COLUMN watch_until timestamptz CHECK (watch_until IS NULL OR kind = 'patch');
