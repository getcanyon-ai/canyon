-- A deploy release's access token (contracts/routes.md, GET /releases/:id/access-token and
-- GET /access/:app/:env; design/HAND-OFF.md §5): what the sidecar in the org's pod pulls its app's
-- access file with. Minted on the pipeline's first fetch. The route answers the same value on every
-- fetch, so the value is sealed as a secret is (0008, vault/) as the row (<project>, "access-token",
-- <release id>), key_ref the key version its data key is wrapped under (0017); access_token_hash is its SHA-256,
-- what the access pull compares, so that pull never opens the vault. All four are cleared on every older
-- release of the environment once a newer one reports deployed: the token dies then (routes.md).
ALTER TABLE releases
    ADD COLUMN key_ref           integer,
    ADD COLUMN sealed_key        bytea,
    ADD COLUMN sealed_value      bytea,
    ADD COLUMN access_token_hash bytea UNIQUE;
