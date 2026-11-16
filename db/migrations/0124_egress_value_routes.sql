-- Layer 3's value routes (design/EGRESS.md §4.1, §4.5; contracts/routes.md, Egress control and Interception, #2137):
-- an app env's routes, each value sealed under the route's own vault row; the interception roots, each key sealed;
-- and each deploy row's trusted roots, which decide whether egress-control intercepts the env's routes.

-- rev: drawn for each write of a route, a PUT or a from-secret, from this one sequence over every route write of
-- the host, so it never repeats for an app env and destination, a route deleted and set again included.
CREATE SEQUENCE egress_route_rev;

-- One route per app env and destination, set whole: its value sealed as a secret is, under (app, env,
-- "route:<destination>"), so no route moves a value to another destination. from_secret: the last write was a
-- from-secret, so only an owner or an admin replaces or deletes it. Never read back but by GET /egress/route-value.
CREATE TABLE egress_routes (
    project_id      text NOT NULL REFERENCES projects (id),
    env             text NOT NULL CHECK (env IN ('preview', 'prod')),
    destination     text NOT NULL CHECK (char_length(destination) BETWEEN 1 AND 253),
    header          text NOT NULL CHECK (header ~ '^[a-z][a-z0-9-]{0,63}$'),
    scheme          text NOT NULL CHECK (scheme IN ('bearer', 'basic', 'raw')),
    placeholder_env text CHECK (placeholder_env ~ '^[A-Za-z_][A-Za-z0-9_]{0,127}$'),
    from_secret     boolean NOT NULL,
    rev             bigint NOT NULL UNIQUE CHECK (rev >= 1),
    key_ref         integer NOT NULL,
    sealed_key      bytea NOT NULL,
    sealed_value    bytea NOT NULL,
    set_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, env, destination)
);

-- The interception roots (5 years), one row a root, each key sealed as a secret is under ("egress", "root",
-- <fingerprint>); the certificate itself is public. The signing root is chosen among them (egressSigningRoot).
CREATE TABLE egress_ca_roots (
    fingerprint  text PRIMARY KEY CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    cert         text NOT NULL,
    key_ref      integer NOT NULL,
    sealed_key   bytea NOT NULL,
    sealed_value bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    not_after    timestamptz NOT NULL
);

-- An env's trusted roots, by SHA-256 fingerprint: the roots every instance of it that may be serving trusts
-- (contracts/routes.md, Interception, Trust). NULL, nothing recorded, is the empty set.
ALTER TABLE deploys ADD COLUMN egress_roots text[];

-- The policy names an env's routes, and only while the signing root is among its trusted roots: a route and a root
-- each move seq when written, in the writing transaction (0096), and a deploy row when its trusted roots change —
-- written otherwise, inserted with none or deleted with none, it changes nothing the policy says, nothing recorded
-- being none (a deploy under via off, a hold and its lift).
CREATE TRIGGER egress_policy_routes AFTER INSERT OR UPDATE OR DELETE ON egress_routes
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_routes_truncated AFTER TRUNCATE ON egress_routes
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_ca_roots AFTER INSERT OR UPDATE OR DELETE ON egress_ca_roots
    FOR EACH ROW EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_ca_roots_truncated AFTER TRUNCATE ON egress_ca_roots
    FOR EACH STATEMENT EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_trusted_roots AFTER UPDATE OF egress_roots ON deploys
    FOR EACH ROW WHEN (coalesce(OLD.egress_roots, '{}') IS DISTINCT FROM coalesce(NEW.egress_roots, '{}'))
    EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_trusted_roots_set AFTER INSERT ON deploys
    FOR EACH ROW WHEN (cardinality(NEW.egress_roots) > 0) EXECUTE FUNCTION egress_policy_moved();
CREATE TRIGGER egress_policy_trusted_roots_gone AFTER DELETE ON deploys
    FOR EACH ROW WHEN (cardinality(OLD.egress_roots) > 0) EXECUTE FUNCTION egress_policy_moved();
