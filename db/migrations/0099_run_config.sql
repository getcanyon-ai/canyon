-- The run-config fingerprint (contracts/routes.md, Patch runs, step 5; design/PATCHING.md §4.3): what the
-- row's /run carried beside its image — the spec, run_by, the saved front door, the runtime secrets' names,
-- versions and set times, the resources' names, never a value — and the sha256 over it. A patch's /run must
-- carry the same; the parts name what changed when it would not. NULL on a row written before it, one the
-- org's report wrote and one a retry wrote on finding its version running: none recorded, and a patch over it
-- fails. run_config_at is when a server that records it wrote the row.
ALTER TABLE deploys ADD COLUMN run_config jsonb, ADD COLUMN run_config_sha256 text CHECK (run_config_sha256 ~ '^[0-9a-f]{64}$'),
    ADD COLUMN run_config_at timestamptz, ADD CHECK ((run_config IS NULL) = (run_config_sha256 IS NULL));

-- A server from before it — a rollback, a rolling upgrade — writes a row again leaving these as they were: a
-- deploy (another run, image or time) or a recreation (another spec) that did not write run_config_at clears
-- them, so a patch over the row fails closed rather than trusting an earlier /run's.
CREATE FUNCTION deploys_run_config_stale() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.deployed_at, NEW.image, NEW.gate_run, NEW.egress) IS DISTINCT FROM (OLD.deployed_at, OLD.image, OLD.gate_run, OLD.egress)
        AND NEW.run_config_at IS NOT DISTINCT FROM OLD.run_config_at THEN
        NEW.run_config := NULL;
        NEW.run_config_sha256 := NULL;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER deploys_run_config_stale BEFORE UPDATE ON deploys FOR EACH ROW EXECUTE FUNCTION deploys_run_config_stale();
