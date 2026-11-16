-- The host's front door on Kubernetes (contracts/routes.md, the front-door row): one per host, one row.
-- door is /run's front_door (contracts/executor.md, The front door) without the sign-in client's secret,
-- which choice 2 alone carries, sealed as a secret is (0008, vault/) as the row ("front-door", "signin",
-- "client_secret"); key_ref is the key version its data key is wrapped under (0017).
CREATE TABLE front_doors (
    id             integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    door           jsonb NOT NULL,
    key_ref        integer,
    sealed_key     bytea,
    sealed_value   bytea,
    set_at         timestamptz NOT NULL DEFAULT now(),
    set_by_kind    text NOT NULL,
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL,
    CHECK (num_nonnulls(key_ref, sealed_key, sealed_value) = CASE WHEN door->>'choice' = '2' THEN 3 ELSE 0 END)
);
