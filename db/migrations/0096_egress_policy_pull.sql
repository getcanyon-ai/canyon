-- The policy egress-control pulls (design/EGRESS.md §3.2, #2113): its version, the app envs it names, the
-- callers the server resolved, the replicas that pull it and the certificate the Kubernetes listener serves.
-- Policy, never secrets: only the certificate's key is one, sealed as a secret is.

-- The policy's version, epoch:seq. seq grows with every row written that can change what the policy says (the
-- triggers below), in the writing transaction, so no route can forget it; epoch is 128 random bits each server
-- start draws again, so a restored database never brings an old version back under its old epoch.
CREATE TABLE egress_policy (
    one   boolean PRIMARY KEY DEFAULT true CHECK (one),
    epoch text NOT NULL DEFAULT md5(random()::text) CHECK (epoch ~ '^[0-9a-f]{32}$'),
    seq   bigint NOT NULL DEFAULT 1 CHECK (seq >= 1)
);
INSERT INTO egress_policy DEFAULT VALUES;

CREATE FUNCTION egress_policy_moved() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE egress_policy SET seq = seq + 1;
    RETURN NULL;
END $$;

-- The app envs the policy names: each Canyon runs under via canyon — named by its deploy step before /run, so
-- the app's first call finds its env, and by its deploy row under via canyon; gone when that row goes, is handed
-- to the org or runs under via off (below), and when a first /run that named it fails.
CREATE TABLE egress_envs (
    project_id text NOT NULL REFERENCES projects (id),
    env        text NOT NULL CHECK (env IN ('preview', 'prod')),
    PRIMARY KEY (project_id, env)
);
INSERT INTO egress_envs SELECT project_id, env FROM deploys WHERE run_by = 'canyon' AND egress->>'via' = 'canyon';

-- The callers: who holds each source address, as the executor's POST /callers answered egress-control's
-- refetch; confirmed again every 15 s, and gone when the answer changes or goes unconfirmed for 30 s.
CREATE TABLE egress_callers (
    address     text PRIMARY KEY CHECK (address ~ '^[0-9.]{7,15}$'),
    project_id  text NOT NULL,
    env         text NOT NULL,
    resolved_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (project_id, env) REFERENCES egress_envs (project_id, env) ON DELETE CASCADE
);

-- The env follows its deploy row, in the row's transaction: a row Canyon runs under via canyon names it — so an
-- undeploy racing a deploy that named it before /run never leaves the app running unnamed — and a row deleted
-- (undeploy, a delete, a hold), handed to the org or run under via off drops it, its callers with it. A row
-- whose spec is unknown (NULL: a retry that started nothing) leaves it as it is.
CREATE FUNCTION egress_env_of_deploy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM egress_envs WHERE project_id = OLD.project_id AND env = OLD.env;
    ELSIF NEW.run_by = 'canyon' AND NEW.egress->>'via' = 'canyon' THEN
        INSERT INTO egress_envs (project_id, env) VALUES (NEW.project_id, NEW.env) ON CONFLICT DO NOTHING;
    ELSIF NEW.run_by <> 'canyon' OR NEW.egress->>'via' = 'off' THEN
        DELETE FROM egress_envs WHERE project_id = NEW.project_id AND env = NEW.env;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER egress_env_of_deploy AFTER INSERT OR UPDATE OF run_by, egress OR DELETE ON deploys
    FOR EACH ROW EXECUTE FUNCTION egress_env_of_deploy();
CREATE FUNCTION egress_envs_gone() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM egress_envs;
    RETURN NULL;
END $$;
CREATE TRIGGER egress_envs_gone AFTER TRUNCATE ON deploys
    FOR EACH STATEMENT EXECUTE FUNCTION egress_envs_gone();

-- Every row written to a table the policy is read from moves seq: the org's via and mode, an app env's mode, its
-- allowlist, the front door (its platform destinations), the envs and the callers. A row rewritten as it was
-- moves it too — a 200, never a stale 304; a statement that writes no row moves nothing. Requests, a caller only
-- confirmed again and a replica seen are nothing the policy says.
CREATE TRIGGER egress_policy_settings AFTER INSERT OR UPDATE OR DELETE ON egress_settings
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_modes AFTER INSERT OR UPDATE OR DELETE ON egress_modes
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_allowed AFTER INSERT OR UPDATE OR DELETE ON egress_allowed
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_front_door AFTER INSERT OR UPDATE OR DELETE ON front_doors
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_envs AFTER INSERT OR UPDATE OR DELETE ON egress_envs
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_callers AFTER INSERT OR UPDATE OF address, project_id, env OR DELETE ON egress_callers
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
-- A TRUNCATE fires no row trigger: each of these tables moves seq on one too, so no way of emptying one leaves a
-- replica holding the old policy under the version it was answered under (a 304 for a policy that changed).
CREATE TRIGGER egress_policy_settings_truncated AFTER TRUNCATE ON egress_settings
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_modes_truncated AFTER TRUNCATE ON egress_modes
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_allowed_truncated AFTER TRUNCATE ON egress_allowed
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_front_door_truncated AFTER TRUNCATE ON front_doors
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_envs_truncated AFTER TRUNCATE ON egress_envs
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_callers_truncated AFTER TRUNCATE ON egress_callers
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();

-- Each egress-control replica that pulls the policy: on Kubernetes the pod its token names, on Docker its
-- address. have is the version it said it holds (NULL: none, or out of shape); current_at the last time that
-- was the current one, or the one last served it, when served; its policy's age is now - current_at.
CREATE TABLE egress_replicas (
    replica      text PRIMARY KEY CHECK (char_length(replica) BETWEEN 1 AND 253),
    have_epoch   text,
    have_seq     bigint,
    seen_at      timestamptz NOT NULL,
    current_at   timestamptz,
    served_epoch text,
    served_seq   bigint,
    served_at    timestamptz
);

-- The certificate the policy's listener serves on Kubernetes, which egress-control pins: minted at the first
-- boot that needs it, its key sealed as a secret is ("egress", "policy", "tls_key"); the certificate itself is
-- public, and canyon-server egress-policy-cert prints it for the install step that publishes it.
CREATE TABLE egress_policy_cert (
    one          boolean PRIMARY KEY DEFAULT true CHECK (one),
    cert         text NOT NULL,
    key_ref      integer NOT NULL,
    sealed_key   bytea NOT NULL,
    sealed_value bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
