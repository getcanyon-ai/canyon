-- An app that built and did not stay up is the user's fix, not Canyon's: failure_reason start
-- (contracts/events.md). Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'start'));
