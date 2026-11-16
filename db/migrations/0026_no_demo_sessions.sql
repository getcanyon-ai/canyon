-- A demo host no longer stores a session for a caller without one (design/AUTH.md): the rows it
-- wrote, one per cookieless request and never expiring, are no longer read, and go.
DELETE FROM sessions WHERE flow = 'demo';
