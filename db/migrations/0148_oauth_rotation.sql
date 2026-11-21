-- A refresh retry is honoured once, and only while the pair its rotation issued is unused (design/AUTH.md §4.7):
-- rotated_unused is true from a rotation until that pair's access token serves a request, the pair is refreshed, or a
-- retry of the token it replaced is honoured. Any other return of a replaced refresh token ends the session.
ALTER TABLE sessions ADD COLUMN rotated_unused boolean NOT NULL DEFAULT false;

-- The address a registration came from, the requester behind a trusted proxy: unused registrations are capped per
-- source, and a registration evicts only its own source's oldest, never another's (design/AUTH.md §4.7).
ALTER TABLE oauth_clients ADD COLUMN source text NOT NULL DEFAULT '';
