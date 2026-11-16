-- When an undeploy with no run in flight began its own /stop of the environment (#2202,
-- contracts/routes.md, The brake, What reads it): the reads leave the row out while the project's
-- undeploy lock is held, so it never reads stopped while it is taken offline. NULL on every other row.
ALTER TABLE deploys ADD COLUMN stopping_since timestamptz;
