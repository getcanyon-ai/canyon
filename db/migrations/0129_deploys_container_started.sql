-- When the deploy step started the container that runs in an environment: the moment before its /run (#2738,
-- contracts/routes.md, the refused predicate). A refusal counts against the running version from then, less a
-- margin, never from the run's start: a preview's review and build take minutes, and the outgoing version's
-- refusals in them were counted as the new one's. NULL on a row written before it, and on the org's: the
-- predicate falls back to run_started_at, then deployed_at.
ALTER TABLE deploys ADD COLUMN container_started_at timestamptz;
