-- Database sessions (design/DATABASE-SESSIONS.md §12; contracts/routes.md, Database sessions): one person
-- × one app database × one mode, opened and ended by Canyon. Expand only: two new tables and a defaulted
-- column. Nothing here is sealed. A row names its sign-in by the session's id, as app_sessions does: that
-- id is the sign-in's cookie value, a secret, never answered or logged, and gone with the session's row.

-- One row per database session. closed_at is set once, by compare-and-set, with close_reason beside it;
-- the counters grow while it is open. revoke_pending marks a closed one whose Revoke failed: any replica's
-- sweep, and the person's next open on the key, run it again until it succeeds. opened_by_session is the sign-in that opened it, null once that
-- sign-in's row is gone — the checks then close it signed_out — and null throughout with sign-in off,
-- where no session row exists.
CREATE TABLE db_sessions (
    id                text PRIMARY KEY CHECK (id ~ '^dbs_[0-9a-f]{16}$'),
    project_id        text NOT NULL REFERENCES projects (id),
    env               text NOT NULL CHECK (env IN ('preview', 'prod')),
    resource          text NOT NULL,
    mode              text NOT NULL CHECK (mode IN ('read', 'write')),
    subject           text NOT NULL,
    display           text NOT NULL DEFAULT '',
    email             text NOT NULL DEFAULT '',
    role              text NOT NULL,
    reason            text,
    opened_by_session text REFERENCES sessions (id) ON DELETE SET NULL,
    relayed_by        text,
    opened_at         timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    last_activity_at  timestamptz NOT NULL DEFAULT now(),
    closed_at         timestamptz,
    close_reason      text CHECK (close_reason IN ('signed_out', 'access_changed', 'setting_changed', 'database_gone', 'expired', 'held', 'idle', 'closed', 'ended', 'replaced')),
    connections       bigint NOT NULL DEFAULT 0,
    queries           bigint NOT NULL DEFAULT 0,
    rows_returned     bigint NOT NULL DEFAULT 0,
    bytes_from_db     bigint NOT NULL DEFAULT 0,
    bytes_to_db       bigint NOT NULL DEFAULT 0,
    revoke_pending    boolean NOT NULL DEFAULT false,
    CHECK ((closed_at IS NULL) = (close_reason IS NULL)),
    CHECK (NOT revoke_pending OR closed_at IS NOT NULL),
    CHECK (env = 'preview' OR reason IS NOT NULL)
);
CREATE INDEX db_sessions_open ON db_sessions (opened_at) WHERE closed_at IS NULL;
CREATE INDEX db_sessions_revoke_pending ON db_sessions (id) WHERE revoke_pending;
CREATE INDEX db_sessions_opened_by ON db_sessions (opened_by_session);   -- a session's delete sets it null
CREATE INDEX db_sessions_opens ON db_sessions (subject, opened_at);       -- a person's opens of the last minute, busy
CREATE UNIQUE INDEX db_sessions_one_open ON db_sessions (project_id, env, resource, subject) WHERE closed_at IS NULL;

-- Each sign-in that opened or attached to a database session, once: where an attach finds whether its
-- sign-in is new, and the checks each connection's sign-in, never read back from the audit rows. Gone
-- with the sign-in's row.
CREATE TABLE db_session_signins (
    db_session text NOT NULL REFERENCES db_sessions (id),
    session    text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    first_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (db_session, session)
);
CREATE INDEX db_session_signins_session ON db_session_signins (session); -- a session's delete cascades here

-- Whether production databases are reachable through database sessions: off by default.
ALTER TABLE org_preferences ADD COLUMN db_prod text NOT NULL DEFAULT 'off' CHECK (db_prod IN ('off', 'read', 'write'));
