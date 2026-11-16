-- Layer 2, Restrict (design/EGRESS.md §3.3–3.5, #2110): the org's default mode, each app env's own mode, its
-- allowlist and its egress requests. Policy, never secrets: no row here holds a credential.

-- The org's default mode, an admin's: every app env without its own follows it.
ALTER TABLE egress_settings ADD COLUMN mode text NOT NULL DEFAULT 'observe' CHECK (mode IN ('observe', 'allowlist'));

-- An app env's own mode, which an owner sets stricter than the org's and an admin either way; no row while the
-- env follows the org's.
CREATE TABLE egress_modes (
    project_id text NOT NULL REFERENCES projects (id),
    env        text NOT NULL CHECK (env IN ('preview', 'prod')),
    mode       text NOT NULL CHECK (mode IN ('observe', 'allowlist')),
    PRIMARY KEY (project_id, env)
);

-- An app env's allowlist: each entry a name or *. and a name, as destination.Entry canonicalised it when it was
-- allowed, so an entry stored before a rule changed is still removed as it is written.
CREATE TABLE egress_allowed (
    project_id text NOT NULL REFERENCES projects (id),
    env        text NOT NULL CHECK (env IN ('preview', 'prod')),
    entry      text NOT NULL CHECK (entry = lower(entry) AND char_length(entry) <= 255), -- *. and a name of 253
    PRIMARY KEY (project_id, env, entry)
);

-- An app env's egress requests (§3.4): the first not_allowed refusal of a destination files one, pending, and
-- each later one counts on it, dismissed or not. An entry that allows the destination closes it — the row goes —
-- so a refusal after that entry is removed files a new one; a dismissed one stays, and files none.
CREATE TABLE egress_requests (
    project_id       text NOT NULL REFERENCES projects (id),
    env              text NOT NULL CHECK (env IN ('preview', 'prod')),
    destination      text NOT NULL CHECK (destination = lower(destination) AND char_length(destination) <= 253),
    state            text NOT NULL CHECK (state IN ('pending', 'dismissed')),
    first_refused_at timestamptz NOT NULL,
    last_refused_at  timestamptz NOT NULL CHECK (last_refused_at >= first_refused_at),
    refusals         bigint NOT NULL CHECK (refusals >= 1),
    PRIMARY KEY (project_id, env, destination)
);
