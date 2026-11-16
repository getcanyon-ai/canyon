-- The brake (contracts/routes.md, The brake): deploys marks a gate run that goes on into build and run,
-- the run writing preview, so an undeploy of preview knows it from a check; stop_requested_* is a stop an
-- undeploy recorded on the run writing its environment past its deploy step, which the run carries out
-- however it ends; ending_at is set, under the ledger lock, as such a run reads that stop on its way to
-- its end, and an undeploy from then on records none and stops what is there itself. deploys defaults to
-- true: a run a server from before this column starts — a rolling upgrade — and every run live as this
-- lands reads as a deploy run, so an undeploy of preview ends it rather than leave it to start what was
-- taken offline; a start from now on writes it.
ALTER TABLE runs
    ADD COLUMN deploys                   boolean NOT NULL DEFAULT true,
    ADD COLUMN stop_requested_at         timestamptz,
    ADD COLUMN stop_requested_by_kind    text CHECK (stop_requested_by_kind IN ('person', 'agent', 'canyon')),
    ADD COLUMN stop_requested_by_subject text,
    ADD COLUMN stop_requested_by_display text,
    ADD COLUMN ending_at                 timestamptz;
