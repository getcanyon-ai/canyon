-- The org's platform webhook (contracts/routes.md, the platform-webhook row; design/HAND-OFF.md
-- §3): one row, one host, where releases go under run_by: org. secret is what Canyon signs every
-- delivery with, sealed as a secret is (0008, vault/) as the row ("platform-webhook", "webhook",
-- "secret"); key_ref is the key version its data key is wrapped under (0017). token_hash is the
-- host token's SHA-256 alone: the plaintext is answered once, by the PUT or the rotate, and never
-- stored.
CREATE TABLE platform_webhook (
    id             integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    url            text NOT NULL,
    key_ref        integer NOT NULL,
    sealed_key     bytea NOT NULL,
    sealed_value   bytea NOT NULL,
    token_hash     bytea NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now(),
    set_by_kind    text NOT NULL,
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL
);
