-- Identity on the session row (design/AUTH.md §5). No role column: the role is computed on every
-- request from the stored email against the current admin list, or claimed_admin, which is stored
-- because the ID token is discarded at sign-in and the claim cannot be re-read. Audit history is
-- frozen, never backfilled; the two deletions below are the only exceptions, and neither rewrites.
ALTER TABLE sessions
    ADD COLUMN issuer     text NOT NULL DEFAULT '',
    ADD COLUMN email      text NOT NULL DEFAULT '',
    ADD COLUMN flow       text NOT NULL DEFAULT 'browser' CHECK (flow IN ('browser', 'device', 'demo')),
    ADD COLUMN claimed_admin boolean NOT NULL DEFAULT false,  -- did the admin group claim match at sign-in
    ADD COLUMN hosted_domain text NOT NULL DEFAULT '',        -- Google's hd claim when present: the provider's word that the account is an organisation's (ONBOARDING.md)
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN revoked_at timestamptz;

DELETE FROM sessions;                                        -- every existing row is anonymous
DELETE FROM project_owners WHERE subject = 'anonymous';      -- an owner who is nobody is not an owner; the demo seed's owners carry email subjects and stay

CREATE TABLE device_grants (
    code_hash    bytea PRIMARY KEY,           -- sha256 of the device code; the code itself is never stored
    user_code    text NOT NULL UNIQUE,        -- 8 characters, Crockford base32, shown to the person
    session_id   text REFERENCES sessions(id) ON DELETE CASCADE, -- set when a person confirms; null while pending; gone with the session, so the boot sweep never trips over a grant
    requested_by text NOT NULL DEFAULT '',    -- the source address, shown on the approval screen
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
