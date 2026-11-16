-- The application version each canyon-server's run driver dequeues, and when it stopped (#2271). A server
-- takes over the queued workflows of a version no row here names, and leaves a running one's to drain. A
-- server stopped cleanly marks its row stopped_at rather than deleting it: its runs move at once, while
-- its version counts as running until the row is stale, so a release rolled back never takes a newer
-- one's work the moment it stops. Every binary before this one runs canyon-v1 and names no version, so
-- that is what its rows say.
ALTER TABLE durable_executors ADD COLUMN version text NOT NULL DEFAULT 'canyon-v1', ADD COLUMN stopped_at timestamptz;
