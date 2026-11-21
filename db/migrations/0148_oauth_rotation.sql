-- A refresh retry is honoured once, and only while the pair its rotation issued is unused (design/AUTH.md §4.7):
-- rotated_unused is true from a rotation until that pair's access token serves a request, the pair is refreshed, or a
-- retry of the token it replaced is honoured. Any other return of a replaced refresh token ends the session.
ALTER TABLE sessions ADD COLUMN rotated_unused boolean NOT NULL DEFAULT false;
-- The refresh token of the pair an honoured retry replaced, which the client never received: if it ever comes back,
-- someone else holds it, and the session ends.
ALTER TABLE sessions ADD COLUMN refresh_lost_hash bytea UNIQUE CHECK (refresh_lost_hash IS NULL OR flow = 'oauth');

-- The source a registration came from, the requester behind a trusted proxy, an IPv4 address or an IPv6 /64: unused
-- registrations are capped per source, and a registration evicts only its own source's oldest (design/AUTH.md §4.7).
ALTER TABLE oauth_clients ADD COLUMN source text NOT NULL DEFAULT '';
