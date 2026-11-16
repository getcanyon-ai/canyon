-- The asker's level on the app when they asked (contracts/routes.md, POST …/access-requests; #2774): none, or viewer, a
-- read-only member asking to join its team. NULL on a request made before it was recorded: never read as none.
ALTER TABLE access_requests ADD COLUMN level text CHECK (level IN ('none', 'viewer'));
