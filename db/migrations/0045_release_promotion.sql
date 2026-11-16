-- A prod release's promotion (design/HAND-OFF.md §3; contracts/routes.md, the promotions row under
-- run_by: org): the gate run preview's environment was deployed from and who deployed it, which the
-- org's deployed report writes on the prod row as a Canyon-run promotion writes them (runs.Pipeline.deploy),
-- the promote run's own actor its promoted_by. Null on a preview release, whose run is its gate run and
-- whose run's actor deployed it.
ALTER TABLE releases
    ADD COLUMN gate_run            text REFERENCES runs(id),
    ADD COLUMN deployed_by_kind    text CHECK (deployed_by_kind IN ('person', 'agent', 'canyon')),
    ADD COLUMN deployed_by_subject text,
    ADD COLUMN deployed_by_display text;
