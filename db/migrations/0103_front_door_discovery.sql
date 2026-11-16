-- Each choice 2 sign-in provider's discovery, by issuer (contracts/routes.md, the front-door row; design/EGRESS.md
-- §3.3): the hosts of the token, user-info and key set endpoints its document names, each as the canonicalisation
-- keeps it — platform destinations of every front door in use naming that issuer. Written by a save whose fetch
-- read the document and by the server's minute loop for an issuer in use with no row; no row is none recorded.
CREATE TABLE front_door_discovery (
    issuer      text PRIMARY KEY,
    hosts       text[] NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);

-- The platform destinations are part of the policy egress-control pulls, so a row written moves its seq (0096).
CREATE TRIGGER egress_policy_front_door_discovery AFTER INSERT OR UPDATE OR DELETE ON front_door_discovery
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_front_door_discovery_truncated AFTER TRUNCATE ON front_door_discovery
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();

-- So is the front door each running deploy was given (its run_config): a deploy's step names its env before the
-- /run, so its row's first write names no egress_envs row anew, nor does a redeploy behind another door or a row
-- whose run_config another release's write cleared: the write itself moves seq, and egress-control holds the
-- door it adds and drops the one it left. Only the door: a Docker host's run_config names none, and a secret's
-- version or a resource changes nothing the policy says.
CREATE TRIGGER egress_policy_deploy_door_inserted AFTER INSERT ON deploys
    FOR EACH ROW WHEN (NEW.run_config->'front_door'->'door' IS NOT NULL) EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_deploy_door AFTER UPDATE ON deploys
    FOR EACH ROW WHEN (OLD.run_config->'front_door'->'door' IS DISTINCT FROM NEW.run_config->'front_door'->'door') EXECUTE FUNCTION egress_policy_moved();

-- Each front door a save replaced, still in use for 30 minutes: a /run that read it before the save may still be
-- starting its sidecars (contracts/routes.md, the front-door row). The server's minute loop deletes one older,
-- which moves seq as the door it replaces drops from the platform destinations.
CREATE TABLE replaced_front_doors (
    door        jsonb NOT NULL,
    replaced_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER egress_policy_replaced_front_doors AFTER INSERT OR UPDATE OR DELETE ON replaced_front_doors
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_replaced_front_doors_truncated AFTER TRUNCATE ON replaced_front_doors
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
