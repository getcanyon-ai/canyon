-- Canyon as the OAuth authorization server for MCP clients (design/AUTH.md §4.7; design/specs/mcp.md §2.2). A
-- client is registered (DCR, oauth_clients) or names itself by a Client ID Metadata Document, which is fetched, not
-- stored. An authorization request waits in oauth_requests for a person's consent in the browser; approving it binds
-- the approver's browser session and a one-minute code, and the token route consumes the code into a session of its
-- own, flow 'oauth', bound to the client and the resource. Each table cleans itself
-- on use, as device_grants does.
-- A request an OAuth session made is done through MCP: the audit row's channel.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_via_check,
    ADD CONSTRAINT audit_events_via_check CHECK (via IN ('dashboard', 'cli', 'github', 'platform', 'canyon', 'mcp'));
ALTER TABLE sessions DROP CONSTRAINT sessions_flow_check,
    ADD CONSTRAINT sessions_flow_check CHECK (flow IN ('browser', 'device', 'demo', 'oauth'));
-- An OAuth session is one row per consent, which lives as long as its refresh token, 30 days at the most: its id is
-- never handed out, so the list's digest of it stays put. The access token (an hour) and the refresh token are each
-- kept as a SHA-256 and rotate on that row in place; the refresh token it replaced is kept too, so a used one presented
-- again ends the session (OAuth 2.1's reuse detection).
ALTER TABLE sessions
    ADD COLUMN oauth_client       text,
    ADD COLUMN oauth_client_name  text,
    ADD COLUMN resource           text,
    ADD COLUMN scope              text[],
    ADD COLUMN access_hash        bytea UNIQUE,
    ADD COLUMN access_expires_at  timestamptz,
    ADD COLUMN refresh_hash       bytea UNIQUE,
    ADD COLUMN refresh_prev_hash  bytea UNIQUE,
    ADD CONSTRAINT sessions_oauth CHECK ((flow = 'oauth') = (oauth_client IS NOT NULL) AND (flow = 'oauth') = (oauth_client_name IS NOT NULL)
        AND (flow = 'oauth') = (resource IS NOT NULL) AND (flow = 'oauth') = (scope IS NOT NULL)
        AND (flow = 'oauth') = (access_hash IS NOT NULL) AND (flow = 'oauth') = (access_expires_at IS NOT NULL)
        AND (flow = 'oauth') = (refresh_hash IS NOT NULL) AND (refresh_prev_hash IS NULL OR flow = 'oauth'));

CREATE TABLE oauth_clients (
    id            text PRIMARY KEY CHECK (id ~ '^dcr_[0-9a-f]{32}$'),
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 10),
    created_at    timestamptz NOT NULL DEFAULT now(),
    used_at       timestamptz -- the first authorization it began: a registration never used within a day is deleted
);

CREATE TABLE oauth_requests (
    id              text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
    client_id       text NOT NULL,
    client_name     text NOT NULL,
    client_host     text NOT NULL, -- a document's host, or a registration's redirect host
    client_verified boolean NOT NULL, -- named by a document at its own https URL, not by a registration's own word
    redirect_uri    text NOT NULL,
    state           text NOT NULL DEFAULT '',
    code_challenge  text NOT NULL CHECK (code_challenge ~ '^[A-Za-z0-9_-]{43}$'),
    scope           text[] NOT NULL CHECK (scope <@ ARRAY['read', 'act'] AND cardinality(scope) > 0),
    resource        text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    session_id      text REFERENCES sessions (id) ON DELETE CASCADE, -- the browser session that allowed it
    code_hash       bytea UNIQUE,
    code_expires_at timestamptz,
    CHECK ((session_id IS NULL) = (code_hash IS NULL) AND (code_hash IS NULL) = (code_expires_at IS NULL))
);
