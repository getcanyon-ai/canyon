-- How a review was asked for (design/specs/mcp.md §7): mcp-local from canyon mcp's Canyon-Transport, mcp-remote for a
-- run the host's own /mcp starts, NULL for the CLI, which sends none, as an older CLI and the dashboard do. Counted,
-- never named further, in the usage report's agents_via.
ALTER TABLE runs ADD COLUMN transport text CHECK (transport IN ('mcp-local', 'mcp-remote'));
