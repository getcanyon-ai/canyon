-- When a deploy Canyon ran in the release's environment first followed the release (contracts/routes.md, the access
-- row and the report row; #2469): a switch back to run_by: canyon, after which the org's pods are no longer the
-- environment's, the release's token pulls the deny-all and a report on it is superseded. Both were read from the
-- environment's deploy row with run_by canyon, which Canyon's undeploy deletes: the token then pulled the app's list
-- again. Recorded on the release, it outlives the row. Expand only: nullable, no default. The backfill records what
-- the rows stop today, so the upgrade reopens nothing.
ALTER TABLE releases ADD COLUMN canyon_since timestamptz;

UPDATE releases r SET canyon_since = d.deployed_at FROM deploys d
WHERE d.project_id = r.project_id AND d.env = r.env AND d.run_by = 'canyon' AND d.deployed_at > r.created_at;

-- Written by the deploy row itself, so every writer of a deploy Canyon ran records it, the release before this one
-- among them during an upgrade: at the deploy's own time, onto each release of the environment made before it and
-- not recorded already. A deploy row is written as Canyon's on its insert, on each deploy that moves deployed_at
-- (runs.upsertDeployTx, a patch run's), and when an org's row goes back to canyon; no other update fires it. It
-- records only while the org's one preferences row says run_by: canyon: a switch back. A write of Canyon's under
-- run_by: org — a patch run's on a row the org's release has not replaced yet, a deploy that read canyon before the
-- switch and wrote after it — records nothing, so the org's newer release is never stopped for good (the security
-- review's M1).
CREATE FUNCTION releases_canyon_since() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE releases SET canyon_since = NEW.deployed_at
    WHERE project_id = NEW.project_id AND env = NEW.env AND created_at < NEW.deployed_at AND canyon_since IS NULL
        AND (SELECT run_by FROM org_preferences) = 'canyon';
    RETURN NULL;
END $$;
CREATE TRIGGER releases_canyon_since AFTER INSERT OR UPDATE OF deployed_at, run_by ON deploys
    FOR EACH ROW WHEN (NEW.run_by = 'canyon') EXECUTE FUNCTION releases_canyon_since();
