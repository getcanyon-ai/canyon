-- Playbook reads (design/specs/mcp.md §3.5; contracts/routes.md, the playbooks row): each fetch of a body through
-- the MCP tool, the resource or canyon playbook, counted per playbook per UTC day per way. A count, naming no one.
CREATE TABLE playbook_reads (
    slug  text    NOT NULL CHECK (slug ~ '^[a-z][a-z0-9-]{0,39}$'),
    day   date    NOT NULL,
    way   text    NOT NULL CHECK (way IN ('tool', 'resource', 'cli')),
    reads bigint  NOT NULL CHECK (reads >= 1),
    PRIMARY KEY (slug, day, way)
);
