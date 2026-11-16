-- Cancelling a run (contracts/routes.md, POST /api/v1/gate-runs/:id/cancel): a running run's workflow
-- reads cancel_requested_at before each step and ends itself failed/interrupted; deploy_started_at is
-- set as its deploy step begins, under the ledger lock the cancel takes, and past it a cancel is too late.
ALTER TABLE runs ADD COLUMN cancel_requested_at timestamptz, ADD COLUMN deploy_started_at timestamptz;
