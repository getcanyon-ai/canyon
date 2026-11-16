-- The minute loop reads each recorded issuer's discovery again once its row is a day old, adding the hosts it names
-- (#2389; contracts/routes.md, the front-door row). refresh_failed_at is when such a read first failed, null once
-- one succeeds: the read's discovery_note says so meanwhile, and every host recorded before stands. The release
-- before writes the row naming issuer and hosts alone, so it reads and writes it as before; in a fleet of both, its
-- save does not clear refresh_failed_at, so a note can outlast a save that read the document by up to a day, until
-- the row is due again and this release's read clears it.
ALTER TABLE front_door_discovery ADD COLUMN refresh_failed_at timestamptz;

-- A row's write moves the policy's version (0103) only when it changes what egress-control pulls: a row added or
-- removed, or its hosts. The policy names neither recorded_at nor refresh_failed_at, so a read that finds nothing
-- new, a failure marked or a recovery moves nothing: each move has every replica fetch the policy again, and
-- deploys wait on that.
DROP TRIGGER egress_policy_front_door_discovery ON front_door_discovery;
CREATE TRIGGER egress_policy_front_door_discovery AFTER INSERT OR DELETE ON front_door_discovery
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_front_door_discovery_hosts AFTER UPDATE ON front_door_discovery
    FOR EACH ROW WHEN (OLD.hosts IS DISTINCT FROM NEW.hosts) EXECUTE FUNCTION egress_policy_moved();
