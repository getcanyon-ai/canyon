-- The reason the org's platform gave in its last failed report on this release (contracts/routes.md,
-- POST /releases/:id/report), written with state failed in the report's own transaction. The run
-- waiting on the release reads it while the state is failed and ends on events.md's line
-- "Your platform reported: <reason>" (runs/release.go, awaitReport).
ALTER TABLE releases ADD COLUMN reason text;
