-- A scan the cluster had no room to run ends the run before the review, capacity and never a finding to
-- approve: failure_reason scan_no_room (contracts/finding.md, Canyon-run tools; #1859).
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start', 'no_room', 'scan_no_room'));
