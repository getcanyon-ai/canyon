-- An app's network as it was last recorded (design/EGRESS.md §2.2, #2254): internal, open, or NULL —
-- unknown, while the executor recreates it or after a recreation it failed. Written unknown before the
-- executor's recreation and resolved once it answers, each in a transaction of its own before any env starts
-- again; and by a /run a deploy actually made — the executor starts one on a network of its egress spec
-- alone — where the network is not what this row says. The app's floor, the networks to recreate and a
-- /run's egress spec read it alone: no row, or an unknown one, is not internal (fail closed).
CREATE TABLE app_networks (
    project_id  text PRIMARY KEY REFERENCES projects(id),
    internal    boolean,
    recorded_at timestamptz NOT NULL DEFAULT now()
);

-- No backfill: via canyon was refused on every host until this migration, so every app's network is open.
