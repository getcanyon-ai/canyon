-- The identity token's signing key (design/PERMISSIONS.md: ES256), generated at the first boot that
-- finds none and sealed as a secret is (0008, vault/) as the row ("identity", "key", kid); key_ref is
-- the key version its data key is wrapped under (0017). kid is the key's RFC 7638 thumbprint, what
-- every token names it by, so a rotation adds a row. The private half never leaves the server.
CREATE TABLE identity_keys (
    kid          text PRIMARY KEY,
    key_ref      integer NOT NULL,
    sealed_key   bytea NOT NULL,
    sealed_value bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
