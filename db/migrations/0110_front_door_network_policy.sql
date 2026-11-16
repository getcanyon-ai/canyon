-- When the save that stored the front door found the cluster enforcing NetworkPolicy: the probe answered network_policy
-- enforced, whatever its certificate; null when it did not, or could not tell (contracts/routes.md, the front-door row;
-- #2507). Canyon builds nothing on a cluster it has not seen enforce it, under either run_by: a build's own
-- NetworkPolicy holds only there. Every door stored before this starts null, probed or not: nothing records which
-- executor probed it, so whether its probe caught a new pod let out before its policy is unknown (fail closed), and an
-- admin saves it once.
ALTER TABLE front_doors ADD COLUMN network_policy_enforced_at timestamptz;

-- The verdict belongs to the save that wrote it. The release before saves a new door as a fresh row, whose verdict is
-- null, but saves the same door again by updating warning, probed_at and probe_fix alone, and moves neither set_at nor
-- the verdict: a verdict left from an earlier save would outlive a probe that found nothing enforced, and a build would
-- run on a cluster nobody checked. So a save that does not write the verdict clears it. This release writes it on every
-- save, now() or null, which differs from the stored one whenever it is a time; an update of other columns, the vault
-- rotation's rewrap among them, leaves it as it is.
CREATE FUNCTION front_door_verdict_unwritten() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.network_policy_enforced_at := NULL;
    RETURN NEW;
END $$;
CREATE TRIGGER front_door_verdict_unwritten BEFORE UPDATE OF warning, probed_at, probe_fix ON front_doors
    FOR EACH ROW WHEN (NEW.network_policy_enforced_at IS NOT DISTINCT FROM OLD.network_policy_enforced_at)
    EXECUTE FUNCTION front_door_verdict_unwritten();
