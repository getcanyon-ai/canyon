-- A pod's check-in (#2452, contracts/routes.md, the access row): when a pod holding this release's access
-- token last pulled its access list, written by the pull itself, best effort and at most once per 30
-- seconds. Unindexed, so the pull's update stays a heap-only one. Expand only: nullable, no default.
ALTER TABLE releases ADD COLUMN seen_at timestamptz;

-- The resources the platform's last deployed report on this release left out (409 resources_missing,
-- #2452), sorted: the environment's pending line names them while the release is open.
ALTER TABLE releases ADD COLUMN missing_resources text[];

-- Each read finds an environment's newest release, and its newest deployed one, by these three.
-- seen_at is in no index, this one included, so the check-in's every-minute update stays HOT.
CREATE INDEX releases_env_newest ON releases (project_id, env, created_at);
