-- The org's chat channels (#2725; contracts/routes.md, the channels row; design/INTEGRATIONS.md): where Canyon posts
-- when production needs a person. The webhook URL is a bearer credential, sealed as a secret is (0008, vault/) as
-- ("integration", <id>, "webhook_url") under key_ref, and no read answers it: host and tail are all Settings shows.
CREATE TABLE notify_channels (
    id             text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{0,39}$'),
    name           text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    kind           text NOT NULL CHECK (kind IN ('slack', 'teams')),
    host           text NOT NULL,
    tail           text NOT NULL,
    key_ref        integer NOT NULL,
    sealed_key     bytea NOT NULL,
    sealed_url     bytea NOT NULL,
    set_by_kind    text NOT NULL,
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now()
);
