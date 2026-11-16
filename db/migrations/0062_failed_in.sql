-- The step a run Canyon itself failed in (contracts/routes.md, A failure reason in words; #1645): set only
-- beside failure_reason error, NULL on every other run and on one that failed before it was recorded.
ALTER TABLE runs ADD COLUMN failed_in text CHECK (failed_in IN ('scan', 'review', 'resources', 'build', 'deploy'));
