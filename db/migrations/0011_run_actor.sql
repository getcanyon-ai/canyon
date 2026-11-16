-- Who started a run (contracts/routes.md, the gate-runs list): the request's actor, recorded onto the row when it is written. Rows from before answer null, no backfill. runs_latest (0009) already serves the list's order.
ALTER TABLE runs
    ADD COLUMN started_by_kind    text CHECK (started_by_kind IN ('person', 'agent', 'canyon')),
    ADD COLUMN started_by_subject text,
    ADD COLUMN started_by_display text;
