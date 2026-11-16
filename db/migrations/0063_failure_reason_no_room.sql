-- A build the cluster had no room to run is capacity, never the package's nor Canyon's: failure_reason
-- no_room (contracts/events.md, contracts/routes.md; #1637).
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start', 'no_room'));
