-- A package no buildpack on the host detects was never compiled: failure_reason detect, never build,
-- and the run keeps what the host builds, and whether a buildpack errored while detecting, for its read
-- (contracts/events.md, contracts/routes.md).
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE runs DROP CONSTRAINT runs_failure_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_failure_reason_check CHECK (failure_reason IN ('interrupted', 'error', 'model', 'build', 'detect', 'start'));
ALTER TABLE runs ADD COLUMN builds text[];
ALTER TABLE runs ADD COLUMN detect_errored boolean NOT NULL DEFAULT false;
